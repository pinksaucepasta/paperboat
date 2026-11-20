package selfhost

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

// repinPlacementsOffNode runs in the drain-and-move transaction, after the node is marked draining: a publication
// pinned to an installation on that node would otherwise have no edge (publication_edge_nodes drops the draining node and
// the pin excludes every other) and its customer DNS would be dropped. Each pin moves to a sibling tunnel installation
// of the same scope on another node publication_edge_nodes accepts (live, edge, http2+http3, verified ingress), preferring the drained node's region, then
// the workspace pair's region order, then installation id; domains retarget exactly as SetPlacement does. With no
// sibling (or no verified domain left) the pin stays: the owner has no other server to serve it.
// shortcut: one transaction for every pin of the node, so a node pinned by thousands of resources is slow; batch then.
func repinPlacementsOffNode(ctx context.Context, tx *db.Tx, node string) error {
	type pin struct{ kind, id, owner, scopeKind, scopeID string }
	rows, err := tx.Query(ctx, `SELECT p.resource_kind,p.resource_id,r.account_id,i.scope_kind,i.scope_id
 FROM paperboat.selfhost_placements p JOIN paperboat.selfhost_installations i ON i.id=p.installation_id
 JOIN LATERAL (SELECT account_id FROM paperboat.preview_leases WHERE p.resource_kind='preview' AND id=p.resource_id
  UNION ALL SELECT account_id FROM paperboat.tunnels WHERE p.resource_kind='tunnel' AND id=p.resource_id) r ON true
 WHERE i.node_id=$1 ORDER BY p.resource_kind,p.resource_id`, node)
	if err != nil {
		return fmt.Errorf("list pinned publications: %w", err)
	}
	var pins []pin
	for rows.Next() {
		var p pin
		if err = rows.Scan(&p.kind, &p.id, &p.owner, &p.scopeKind, &p.scopeID); err != nil {
			rows.Close()
			return err
		}
		pins = append(pins, p)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}
	for _, p := range pins {
		var sibling string
		err = tx.QueryRow(ctx, `SELECT i.id FROM paperboat.selfhost_installations i
 JOIN paperboat.control_tunnel_nodes n ON n.id=i.node_id
 LEFT JOIN paperboat.workspace_edge_sets s ON s.scope_kind=CASE WHEN paperboat.publication_workspace_team($1,$2) IS NULL THEN 'account' ELSE 'team' END
  AND s.scope_id=coalesce(paperboat.publication_workspace_team($1,$2),$3)
 WHERE i.capability='tunnel' AND i.state='active' AND i.claim_complete AND (i.scope_kind,i.scope_id)=($4,$5) AND i.node_id<>$6
 AND n.state='ready' AND n.ready AND n.drain_deadline IS NULL AND n.last_heartbeat_at>now()-interval '15 seconds' AND n.registry_expires_at>now()
 AND n.roles @> ARRAY['edge']::text[] AND n.transports @> ARRAY['http2','http3']::text[]
 AND n.public_ingress_verified_at IS NOT NULL AND (n.public_ingress_ipv4 IS NOT NULL OR n.public_ingress_ipv6 IS NOT NULL)
 AND paperboat.node_workspace_edge_eligible(i.node_id,$3,paperboat.publication_workspace_team($1,$2))
 ORDER BY (n.region IS DISTINCT FROM (SELECT region FROM paperboat.control_tunnel_nodes WHERE id=$6)),coalesce(array_position(s.regions,n.region),9),i.id LIMIT 1`,
			p.kind, p.id, p.owner, p.scopeKind, p.scopeID, node).Scan(&sibling)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return fmt.Errorf("find a sibling server for %s %s: %w", p.kind, p.id, err)
		}
		table, err := resourceTable(p.kind)
		if err != nil {
			return err
		}
		if _, err = setPlacementTx(ctx, tx, p.owner, p.kind, table, p.id, &sibling); err != nil && !errors.Is(err, ErrPlacementDomainRequired) {
			return fmt.Errorf("move %s %s to a sibling server: %w", p.kind, p.id, err)
		}
	}
	return nil
}
