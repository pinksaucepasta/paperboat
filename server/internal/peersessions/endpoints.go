package peersessions

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"slices"
	"time"
)

// MaxReportedEndpoints is how many candidate endpoints an engine may report (PLAN 3b, N5).
const MaxReportedEndpoints = 8

// normalizeEndpoints is the stored form of an engine's reported candidate endpoints: each a unicast ip:port
// (IPv4-mapped addresses unwrapped; no unspecified, loopback or multicast address, no port 0), sorted and
// deduplicated, so equal sets compare equal. nil (not reported) stays nil; an empty report is an empty set.
func normalizeEndpoints(reported []string) ([]string, error) {
	if reported == nil {
		return nil, nil
	}
	if len(reported) > MaxReportedEndpoints {
		return nil, ErrInvalid
	}
	out := make([]string, 0, len(reported))
	for _, raw := range reported {
		endpoint, err := netip.ParseAddrPort(raw)
		if err != nil {
			return nil, ErrInvalid
		}
		addr := endpoint.Addr().Unmap()
		if !addr.IsValid() || addr.IsUnspecified() || addr.IsLoopback() || addr.IsMulticast() || addr.Zone() != "" || endpoint.Port() == 0 {
			return nil, ErrInvalid
		}
		out = append(out, netip.AddrPortFrom(addr, endpoint.Port()).String())
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}

// endpointPublishInterval is how often an identity's delivered endpoints may change: a set reported inside it is
// held back until the interval has passed since the last publication and then published by the server itself
// (PublishDueEndpoints), so a flapping NAT publishes peer_network at most once per interval and the latest set wins.
const endpointPublishInterval = time.Minute

// endpointsUpdate is the identity row write of a configuration request (one statement, the generation bump): it
// stores the reported set ($4, NULL when none was sent) and marks the identity capable when a set was sent. When
// the latest set differs from the delivered one it is published now if the last publication is older than $5
// seconds, else held back until then (endpoints_due_at); a set equal to the delivered one clears a held-back one.
// NULL and an empty set are the same set. The delivered set's change publishes peer_network (migration 186).
const endpointsUpdate = `UPDATE paperboat.peer_network_identities SET config_generation=config_generation+1,updated_at=$1,
 endpoints=coalesce($4::text[],endpoints),
 endpoints_capable=endpoints_capable OR $4::text[] IS NOT NULL,
 published_endpoints=CASE WHEN ` + endpointsDiffer + ` AND ` + endpointsOpen + ` THEN coalesce($4::text[],endpoints) ELSE published_endpoints END,
 endpoints_published_at=CASE WHEN ` + endpointsDiffer + ` AND ` + endpointsOpen + ` THEN $1 ELSE endpoints_published_at END,
 endpoints_due_at=CASE WHEN ` + endpointsDiffer + ` AND NOT ` + endpointsOpen + ` THEN endpoints_published_at+make_interval(secs=>$5) END
 WHERE user_id=$2 AND endpoint_id=$3 RETURNING config_generation,endpoints_capable`

const (
	endpointsDiffer = `coalesce($4::text[],endpoints,'{}') IS DISTINCT FROM coalesce(published_endpoints,'{}')`
	endpointsOpen   = `(endpoints_published_at IS NULL OR endpoints_published_at<=$1::timestamptz-make_interval(secs=>$5))`
)

// dueEndpointsBatch bounds one PublishDueEndpoints statement.
const dueEndpointsBatch = 500

// PublishDueEndpoints publishes the held-back endpoint sets whose interval has passed, at most dueEndpointsBatch
// per statement until none is due, and returns how many it published. The leader runs it every worker interval.
func (s *NetworkService) PublishDueEndpoints(ctx context.Context) (int, error) {
	total := 0
	for {
		result, err := s.store.SQL().ExecContext(ctx, `UPDATE paperboat.peer_network_identities n
 SET published_endpoints=coalesce(n.endpoints,'{}'),endpoints_published_at=$1,endpoints_due_at=NULL
 FROM (SELECT user_id,endpoint_id FROM paperboat.peer_network_identities WHERE endpoints_due_at<=$1
  ORDER BY endpoints_due_at LIMIT $2 FOR UPDATE SKIP LOCKED) due
 WHERE n.user_id=due.user_id AND n.endpoint_id=due.endpoint_id`, s.now().UTC(), dueEndpointsBatch)
		if err != nil {
			return total, fmt.Errorf("publish due peer endpoints: %w", err)
		}
		n, err := result.RowsAffected()
		if err != nil {
			return total, fmt.Errorf("publish due peer endpoints: %w", err)
		}
		total += int(n)
		if n < dueEndpointsBatch {
			return total, nil
		}
	}
}

// publishedEndpoints decodes array_to_json(published_endpoints); an empty set is none.
func publishedEndpoints(raw []byte) ([]string, error) {
	var out []string
	if len(raw) == 0 {
		return nil, nil
	}
	if err := json.Unmarshal(raw, &out); err != nil || len(out) == 0 {
		return nil, err
	}
	return out, nil
}

// deliverEndpoints moves each peer's delivered endpoints from its identity, where loading and the mesh log keep
// them, onto its entry, and only for a recipient that declared it accepts them: an engine that never sent an
// endpoints field refuses the unknown field in the signed configuration.
func deliverEndpoints(peers []networkPeer, capable bool) {
	for i := range peers {
		if capable {
			peers[i].Endpoints = peers[i].Identity.Endpoints
		}
		peers[i].Identity.Endpoints = nil
	}
}

// configurationClaims are the signed network configuration's claims for one recipient.
func configurationClaims(issuer string, now, expiry time.Time, generation int64, self networkBinding, peers []networkPeer, endpointsCapable bool) map[string]any {
	deliverEndpoints(peers, endpointsCapable)
	self.Endpoints = nil
	return map[string]any{"version": 1, "iss": issuer, "aud": "paperboat-network", "iat": now.Unix(), "exp": expiry.Unix(), "generation": generation, "self": bindingMap(self), "peers": peers}
}
