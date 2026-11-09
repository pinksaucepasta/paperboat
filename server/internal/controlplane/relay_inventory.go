package controlplane

import (
	"context"
	"database/sql"
	"time"
)

// HostedRelay is one relay the account's machines can use: Paperboat's or one of its own self-hosted
// ones. Like HostedEdge, its status is an observation from the node's heartbeat, not a promise that a
// particular connection will be relayed.
type HostedRelay struct {
	ID            string     `json:"id"`
	Name          string     `json:"name,omitempty"` // a self-hosted relay's name
	Region        string     `json:"region"`
	Status        string     `json:"status"`
	SelfHosted    bool       `json:"self_hosted"`
	LastHeartbeat *time.Time `json:"last_heartbeat_at,omitempty"`
}

type HostedRelayPage struct {
	Items      []HostedRelay `json:"items"`
	NextCursor string        `json:"next_cursor,omitempty"`
	ObservedAt time.Time     `json:"observed_at"`
}

// ListHostedRelays lists the relay nodes the account's pool policy lets it use (paperboat.node_pool_eligible,
// the same rule that signs relay grants), ready only on the grant rules' freshness (relayGrants in peersessions).
func (s *EdgeService) ListHostedRelays(ctx context.Context, accountID, after string, limit int) (HostedRelayPage, error) {
	if accountID == "" || len(after) > 128 || limit < 1 || limit > 100 {
		return HostedRelayPage{}, ErrInvalidEdgePage
	}
	at := s.clock().UTC()
	page := HostedRelayPage{Items: []HostedRelay{}, ObservedAt: at}
	rows, err := s.store.SQL().QueryContext(ctx, `
SELECT n.id, COALESCE(n.region,''),
  COALESCE((SELECT i.name FROM selfhost_installations i WHERE i.node_id=n.id AND i.capability='relay' AND i.scope_kind IN ('account','team') LIMIT 1),''),
  EXISTS (SELECT 1 FROM selfhost_installations i WHERE i.node_id=n.id AND i.scope_kind IN ('account','team')),
  CASE WHEN n.state='draining' OR n.drain_deadline IS NOT NULL THEN 'draining'
       WHEN n.state='ready' AND n.ready=true
         AND n.last_heartbeat_at>$3::timestamptz-interval '15 seconds'
         AND n.registry_expires_at>$3::timestamptz
         AND n.transports @> ARRAY['derp_quic']::text[]
         THEN 'ready' ELSE 'unavailable' END,
  n.last_heartbeat_at
FROM control_tunnel_nodes n
WHERE n.roles @> ARRAY['relay']::text[] AND n.id>$2 AND paperboat.node_pool_eligible(n.id,$1,'relay')
ORDER BY n.id LIMIT $4`, accountID, after, at, limit+1)
	if err != nil {
		return HostedRelayPage{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var relay HostedRelay
		var heartbeat sql.NullTime
		if err := rows.Scan(&relay.ID, &relay.Region, &relay.Name, &relay.SelfHosted, &relay.Status, &heartbeat); err != nil {
			return HostedRelayPage{}, err
		}
		if heartbeat.Valid {
			value := heartbeat.Time.UTC()
			relay.LastHeartbeat = &value
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
