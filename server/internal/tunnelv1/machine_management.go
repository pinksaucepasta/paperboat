package tunnelv1

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/previewtunnelapi"
	"github.com/pinksaucepasta/paperboat/server/internal/teams"
	"github.com/pinksaucepasta/paperboat/server/internal/workspace"
)

var ErrMachinePublicationDenied = errors.New("shared machine management does not grant public publication or connector enrollment")

type tunnelManagementRepository interface {
	ResolveManagementAccount(context.Context, string, string) (string, bool, error)
}
type tunnelManagementKey struct{}
type tunnelManagementScope struct{ actor, owner, tunnel string }

func (r *SQLRepository) ResolveManagementAccount(ctx context.Context, actor, tunnel string) (string, bool, error) {
	var owner string
	var shared bool
	ws, selected := workspace.FromContext(ctx)
	err := r.db.Pool().QueryRow(ctx, `SELECT t.account_id,NOT(t.account_id=$1 AND m.owner_team_id IS NULL AND m.user_id=$1) FROM paperboat.tunnels t JOIN paperboat.user_machines m ON m.id=t.created_by_host_id WHERE t.id=$2 AND paperboat.tunnel_management_allowed($1,t.id) AND (NOT $3 OR paperboat.workspace_tunnel_management_allowed($1,$4,t.id))`, actor, tunnel, selected, nullableString(ws.TeamID)).Scan(&owner, &shared)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, ErrNotFound
	}
	return owner, shared, err
}
func scopeTunnelManagement(ctx context.Context, repository tunnelManagementRepository, request previewtunnelapi.RequestContext, tunnel string) (context.Context, previewtunnelapi.RequestContext, bool, error) {
	actor := request.Actor.AccountID
	owner, shared, err := repository.ResolveManagementAccount(ctx, actor, tunnel)
	if err != nil {
		return ctx, request, false, err
	}
	// AccountID addresses the persisted resource; ActorID continues to name the
	// authenticated actor for authorization and audit. Neither is a client field.
	request.Actor.AccountID = owner
	return context.WithValue(ctx, tunnelManagementKey{}, tunnelManagementScope{actor: actor, owner: owner, tunnel: tunnel}), request, shared, nil
}
func authorizeTunnelManagementTx(ctx context.Context, tx *db.Tx) error {
	scope, ok := ctx.Value(tunnelManagementKey{}).(tunnelManagementScope)
	if !ok {
		return nil
	}
	if err := teams.Lock(ctx, tx); err != nil {
		return err
	}
	var allowed bool
	ws, selected := workspace.FromContext(ctx)
	err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM tunnels WHERE id=$1 AND account_id=$2 AND tunnel_management_allowed($3,id) AND (NOT $4 OR workspace_tunnel_management_allowed($3,$5,id)))`, scope.tunnel, scope.owner, scope.actor, selected, nullableString(ws.TeamID)).Scan(&allowed)
	if err != nil {
		return err
	}
	if !allowed {
		return ErrNotFound
	}
	return nil
}
