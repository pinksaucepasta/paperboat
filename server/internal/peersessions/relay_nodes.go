package peersessions

import (
	"context"
	"database/sql"
	"slices"
	"sync"
	"time"
)

// relayNodeTTL is how long one account's eligible relay nodes are reused across configuration requests. The rows
// are heartbeat state (a node counts while it reported in the last 15 s), so a few seconds keep them as fresh as
// the registry while an idle fleet reads them once per account per interval instead of twice per request.
const relayNodeTTL = 5 * time.Second

// maxRelayNodeAccounts bounds the cache; past it, expired entries are swept.
const maxRelayNodeAccounts = 10_000

// maximumRelayNodeRows bounds one read: more than the regional candidates and the 32 relays a grant set names.
const maximumRelayNodeRows = 256

// relayNodeRow is one ready relay node an account may use, with the columns regional candidates and relay grants read.
type relayNodeRow struct {
	id, processEpoch, region, failureDomain, host, state string
	nodeGeneration                                       int64
	roles, transports, pin, peerRelayWG, peerRelayDisco  []byte
	tcpPort, quicPort                                    int
	observed, expires, capacityAt                        time.Time
	capacityLimit, capacityUsed                          uint64
	drain                                                sql.NullTime
	peerRelayAddress                                     sql.NullString
	selfHosted, relay, peerRelay                         bool
}

type relayNodeEntry struct {
	at   time.Time
	rows []relayNodeRow
}

type relayNodeCache struct {
	mu      sync.Mutex
	entries map[string]relayNodeEntry
}

// relayNodes are the ready relay nodes account may use, in (region, id) order, at most maximumRelayNodeRows. A managed
// relay the load rule holds out (paperboat.node_load_out, decided by the worker leader: above the load-out percent, never
// the last of a region) is left out of the list: clients rank relays by measured round trip only, so leaving a relay
// out is the one way the server steers load. The cache TTL bounds how fast the list reacts.

func (s *NetworkService) relayNodes(ctx context.Context, account string, now time.Time) ([]relayNodeRow, error) {
	s.nodes.mu.Lock()
	e, ok := s.nodes.entries[account]
	s.nodes.mu.Unlock()
	if ok && now.Sub(e.at) >= 0 && now.Sub(e.at) < relayNodeTTL {
		return e.rows, nil
	}
	rows, err := s.store.SQL().QueryContext(ctx, `SELECT id,node_generation,process_epoch,region,failure_domain,array_to_json(roles),array_to_json(transports),coalesce(endpoint_host,''),coalesce(endpoint_tcp_port,0),coalesce(endpoint_quic_port,0),state,last_heartbeat_at,registry_expires_at,capacity_limit,capacity_used,capacity_observed_at,drain_deadline,tls_spki_sha256,
 EXISTS(SELECT 1 FROM paperboat.selfhost_installations i WHERE i.node_id=control_tunnel_nodes.id AND i.scope_kind IN ('account','team')),
 roles @> ARRAY['relay']::text[] AND transports @> ARRAY['derp_quic']::text[],roles @> ARRAY['peer_relay']::text[],peer_relay_wireguard_public_key,peer_relay_disco_public_key,host(peer_relay_virtual_address)
 FROM paperboat.control_tunnel_nodes WHERE region IS NOT NULL AND paperboat.node_pool_eligible(id,$1,'relay') AND state='ready' AND ready AND last_heartbeat_at>$2::timestamptz-interval '15 seconds' AND registry_expires_at>$2::timestamptz AND capacity_observed_at>$2::timestamptz-interval '15 seconds' AND capacity_used<capacity_limit-capacity_limit/10 AND drain_deadline IS NULL AND NOT EXISTS (SELECT 1 FROM paperboat.node_load_out l WHERE l.node_id=control_tunnel_nodes.id AND l.out_for_load) ORDER BY region,id LIMIT $3`,
		account, now, maximumRelayNodeRows)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]relayNodeRow, 0, 8)
	for rows.Next() {
		var n relayNodeRow
		if err := rows.Scan(&n.id, &n.nodeGeneration, &n.processEpoch, &n.region, &n.failureDomain, &n.roles, &n.transports, &n.host, &n.tcpPort, &n.quicPort, &n.state, &n.observed, &n.expires, &n.capacityLimit, &n.capacityUsed, &n.capacityAt, &n.drain, &n.pin,
			&n.selfHosted, &n.relay, &n.peerRelay, &n.peerRelayWG, &n.peerRelayDisco, &n.peerRelayAddress); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	s.nodes.mu.Lock()
	if s.nodes.entries == nil || len(s.nodes.entries) >= maxRelayNodeAccounts {
		s.nodes.entries = freshRelayNodes(s.nodes.entries, now)
	}
	s.nodes.entries[account] = relayNodeEntry{at: now, rows: slices.Clip(out)}
	s.nodes.mu.Unlock()
	return out, nil
}

// freshRelayNodes keeps the unexpired entries (a fresh map when every entry is still fresh and the bound is reached).
func freshRelayNodes(entries map[string]relayNodeEntry, now time.Time) map[string]relayNodeEntry {
	kept := make(map[string]relayNodeEntry, len(entries))
	for k, e := range entries {
		if now.Sub(e.at) < relayNodeTTL {
			kept[k] = e
		}
	}
	if len(kept) >= maxRelayNodeAccounts {
		return map[string]relayNodeEntry{}
	}
	return kept
}
