package selfhost

import (
	"context"

	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/edgeset"
)

// syncTunnelPoolSQL (args: scope kind, scope id) makes a self-hosted-only workspace's tunnel pool every own tunnel
// server of its pair's regions: the pair names regions, so a server enrolled into a region joins the pool by itself
// and a removed one leaves it. A workspace on Paperboat's edges has no pool to keep (no row matches).
// shortcut: the pool is capped at the policy's 32 servers (first 32 by id); raise the policy limit to serve more.
const syncTunnelPoolSQL = `UPDATE paperboat.selfhost_pool_policies p SET installation_ids=ARRAY(
 SELECT i.id FROM paperboat.selfhost_installations i JOIN paperboat.control_tunnel_nodes n ON n.id=i.node_id
 WHERE i.capability='tunnel' AND i.state='active' AND i.claim_complete AND i.scope_kind<>'global'
  AND ((i.scope_kind=p.scope_kind AND i.scope_id=p.scope_id) OR (p.scope_kind='account' AND paperboat.selfhost_scope_visible(i.scope_kind,i.scope_id,p.scope_id)))
  AND n.region=ANY(s.regions) ORDER BY i.id LIMIT 32)
FROM paperboat.workspace_edge_sets s
WHERE p.capability='tunnel' AND p.mode='self-hosted-only' AND p.scope_kind=$1 AND p.scope_id=$2 AND s.scope_kind=p.scope_kind AND s.scope_id=p.scope_id`

func syncTunnelPool(ctx context.Context, tx *db.Tx, ws edgeset.Scope) error {
	_, err := tx.Exec(ctx, syncTunnelPoolSQL, ws.Kind, ws.ID)
	return err
}
