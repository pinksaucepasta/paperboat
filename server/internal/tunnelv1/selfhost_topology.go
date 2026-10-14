package tunnelv1

import (
	"context"
	"fmt"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
	"github.com/pinksaucepasta/paperboat-server/internal/workspace"
)

var ErrSelfhostHTTPDomainRequired = fmt.Errorf("%w: self-hosted-only tunnel pools cannot serve a Paperboat-managed HTTP URL; configure and verify a custom domain, or select managed edge mode", ErrInvalidInput)

func rejectManagedHTTPForSelfhostOnly(ctx context.Context, tx *db.Tx, account, protocol, matchType, routeID string) error {
	if protocol != "http" || matchType != "managed" {
		return nil
	}
	ws, _ := workspace.FromContext(ctx)
	var only bool
	if err := tx.QueryRow(ctx, `SELECT paperboat.workspace_edge_mode($1,COALESCE($2::text,(SELECT paperboat.publication_workspace_team('tunnel',tunnel_id) FROM paperboat.tunnel_routes WHERE id=$3)))='self-hosted-only'`, account, nullableString(ws.TeamID), routeID).Scan(&only); err != nil {
		return err
	}
	if !only {
		return nil
	}
	if routeID != "" {
		var verified bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM paperboat.tunnel_domains WHERE account_id=$1 AND route_id=$2 AND deleted_at IS NULL AND ownership_state='verified' AND conflict_state='clear')`, account, routeID).Scan(&verified); err != nil {
			return err
		}
		if verified {
			return nil
		}
	}
	return ErrSelfhostHTTPDomainRequired
}
