package controlplane

import (
	"context"
	"time"
)

// HostedRelay is one relay region the account's machines can use (Paperboat's or one of its own self-hosted ones; a
// region is a set of relay nodes the machines do not choose between). Like HostedEdge, its status is an observation
// folded over the region's nodes from their heartbeats, not a promise that a particular connection will be relayed.
// Name is Paperboat's display name for a managed region and the region id for the account's own.
type HostedRelay struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
}

type HostedRelayPage struct {
	Items      []HostedRelay `json:"items"`
	NextCursor string        `json:"next_cursor,omitempty"`
	ObservedAt time.Time     `json:"observed_at"`
}

// ListHostedRelays lists, by region id, the regions of the relay nodes the account's pool policy lets it use
// (paperboat.node_pool_eligible, the same rule that signs relay grants), ready only on the grant rules' freshness
// (relayGrants in peersessions).
func (s *EdgeService) ListHostedRelays(ctx context.Context, accountID, after string, limit int) (HostedRelayPage, error) {
	if accountID == "" || len(after) > 128 || limit < 1 || limit > 100 {
		return HostedRelayPage{}, ErrInvalidEdgePage
	}
	at := s.clock().UTC()
	page := HostedRelayPage{Items: []HostedRelay{}, ObservedAt: at}
	rows, err := s.store.SQL().QueryContext(ctx, `
SELECT v.region, coalesce((SELECT r.display_name FROM operator_regions r WHERE r.id=v.region),v.region), `+foldStatusSQL+`
FROM (
  SELECT n.region,
    CASE WHEN n.state='draining' OR n.drain_deadline IS NOT NULL THEN 'draining'
         WHEN n.state='ready' AND n.ready=true
           AND n.last_heartbeat_at>$3::timestamptz-interval '15 seconds'
           AND n.registry_expires_at>$3::timestamptz
           AND n.transports @> ARRAY['derp_quic']::text[]
           THEN 'ready' ELSE 'unavailable' END AS status
  FROM control_tunnel_nodes n
  WHERE n.roles @> ARRAY['relay']::text[] AND n.region IS NOT NULL AND n.region>$2 AND n.state<>'retired'
    AND paperboat.node_pool_eligible(n.id,$1,'relay')
) v
GROUP BY v.region ORDER BY v.region LIMIT $4`, accountID, after, at, limit+1)
	if err != nil {
		return HostedRelayPage{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var relay HostedRelay
		if err := rows.Scan(&relay.ID, &relay.Name, &relay.Status); err != nil {
			return HostedRelayPage{}, err
		}
		if len(page.Items) == limit {
			page.NextCursor = page.Items[len(page.Items)-1].ID
			break
		}
		page.Items = append(page.Items, relay)
	}
	if err := rows.Err(); err != nil {
		return HostedRelayPage{}, err
	}
	return page, nil
}
