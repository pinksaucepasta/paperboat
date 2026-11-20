package controlplane

import (
	"context"
	"errors"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/edgeset"
)

var ErrInvalidEdgePage = errors.New("invalid edge page")

// HostedEdge is one Paperboat region as an account sees it: where its tunnels and previews can be served from. Users
// see regions only (prior art: tailscale tailcfg.DERPRegion groups the nodes a client does not choose between); the
// nodes, their hosts and heartbeats never leave the server. Status is an observation folded over the region's nodes,
// not a promise that a particular route will be admitted: ready if any node is ready, else draining if any is,
// else unavailable.
type HostedEdge struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
}

type HostedEdgePage struct {
	Items      []HostedEdge `json:"items"`
	NextCursor string       `json:"next_cursor,omitempty"`
	ObservedAt time.Time    `json:"observed_at"`
}

// foldStatusSQL turns the per-node statuses of a GROUP BY region into the region's status.
const foldStatusSQL = `CASE WHEN bool_or(v.status='ready') THEN 'ready' WHEN bool_or(v.status='draining') THEN 'draining' ELSE 'unavailable' END`

// ListHostedEdges lists the enabled Paperboat regions that have a non-retired edge node the account may see, by id.
func (s *EdgeService) ListHostedEdges(ctx context.Context, accountID, after string, limit int) (HostedEdgePage, error) {
	if accountID == "" || len(after) > 128 || limit < 1 || limit > 100 {
		return HostedEdgePage{}, ErrInvalidEdgePage
	}
	at := s.clock().UTC()
	page := HostedEdgePage{Items: []HostedEdge{}, ObservedAt: at}
	rows, err := s.store.SQL().QueryContext(ctx, `
SELECT r.id, r.display_name, `+foldStatusSQL+`
FROM operator_regions r JOIN (
  SELECT n.region, `+edgeset.NodeStatusSQL("$3")+` AS status
  FROM control_tunnel_nodes n
  WHERE n.roles @> ARRAY['edge']::text[] AND n.state<>'retired'
    AND NOT EXISTS (SELECT 1 FROM selfhost_installations i WHERE i.node_id=n.id
        AND (i.scope_kind <> 'global' OR i.state <> 'active' OR NOT i.claim_complete))
    AND (n.allowed_account_ids IS NULL OR $1=ANY(n.allowed_account_ids))
    AND NOT EXISTS (SELECT 1 FROM managed_node_authority m WHERE m.node_id=n.id AND m.retired_at IS NOT NULL)
) v ON v.region=r.id
WHERE r.enabled AND r.id>$2
GROUP BY r.id, r.display_name ORDER BY r.id LIMIT $4`, accountID, after, at, limit+1)
	if err != nil {
		return HostedEdgePage{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var edge HostedEdge
		if err := rows.Scan(&edge.ID, &edge.Name, &edge.Status); err != nil {
			return HostedEdgePage{}, err
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
