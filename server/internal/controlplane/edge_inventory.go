package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

var ErrInvalidEdgePage = errors.New("invalid edge page")

// HostedEdge contains only account-visible public inventory fields. Its status
// is an observation, not a promise that a particular route will be admitted.
type HostedEdge struct {
	ID            string     `json:"id"`
	Region        string     `json:"region"`
	Status        string     `json:"status"`
	LastHeartbeat *time.Time `json:"last_heartbeat_at,omitempty"`
}

type HostedEdgePage struct {
	Items      []HostedEdge `json:"items"`
	NextCursor string       `json:"next_cursor,omitempty"`
	ObservedAt time.Time    `json:"observed_at"`
}

func (s *EdgeService) ListHostedEdges(ctx context.Context, accountID, after string, limit int) (HostedEdgePage, error) {
	if accountID == "" || len(after) > 128 || limit < 1 || limit > 100 {
		return HostedEdgePage{}, ErrInvalidEdgePage
	}
	at := s.clock().UTC()
	page := HostedEdgePage{Items: []HostedEdge{}, ObservedAt: at}
	rows, err := s.store.SQL().QueryContext(ctx, `
SELECT n.id, COALESCE(n.region,''),
  CASE WHEN n.state='draining' OR n.drain_deadline IS NOT NULL THEN 'draining'
       WHEN n.state='ready' AND n.ready=true
         AND n.last_heartbeat_at>$3::timestamptz-interval '15 seconds'
		 AND n.registry_expires_at>$3::timestamptz
		 AND n.transports @> ARRAY['http3','http2']::text[]
		 AND n.carrier_endpoint_host IS NOT NULL
		 AND n.carrier_endpoint_tcp_port IS NOT NULL
		 AND n.carrier_endpoint_quic_port IS NOT NULL
		 AND n.carrier_server_spki_sha256 IS NOT NULL
		 AND n.carrier_server_certificate_chain_pem IS NOT NULL
		 THEN 'ready' ELSE 'unavailable' END,
  n.last_heartbeat_at
FROM control_tunnel_nodes n
WHERE n.roles @> ARRAY['edge']::text[] AND n.id>$2
  AND NOT EXISTS (SELECT 1 FROM selfhost_installations i WHERE i.node_id=n.id)
  AND (n.allowed_account_ids IS NULL OR $1=ANY(n.allowed_account_ids))
  AND (NOT EXISTS (SELECT 1 FROM managed_node_authority m WHERE m.node_id=n.id)
       OR EXISTS (SELECT 1 FROM managed_node_authority m JOIN operator_regions r ON r.id=m.region_id
                  WHERE m.node_id=n.id AND m.retired_at IS NULL AND r.enabled))
ORDER BY n.id LIMIT $4`, accountID, after, at, limit+1)
	if err != nil {
		return HostedEdgePage{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var edge HostedEdge
		var heartbeat sql.NullTime
		if err := rows.Scan(&edge.ID, &edge.Region, &edge.Status, &heartbeat); err != nil {
			return HostedEdgePage{}, err
		}
		if heartbeat.Valid {
			value := heartbeat.Time.UTC()
			edge.LastHeartbeat = &value
		}
		if len(page.Items) == limit {
			page.NextCursor = page.Items[len(page.Items)-1].ID
			break
		}
		page.Items = append(page.Items, edge)
	}
	if err := rows.Err(); err != nil {
		return HostedEdgePage{}, err
	}
	return page, nil
}
