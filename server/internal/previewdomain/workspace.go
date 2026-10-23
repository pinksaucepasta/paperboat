package previewdomain

import (
	"context"
	"database/sql"
	"github.com/jackc/pgx/v5"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/previewtunnelapi"
	"github.com/pinksaucepasta/paperboat/server/internal/teams"
	"github.com/pinksaucepasta/paperboat/server/internal/workspace"
)

type workspaceManagementActorKey struct{}

func (r *SQLRepository) ScopeWorkspacePreview(ctx context.Context, actor, preview string) (context.Context, string, error) {
	scope, selected := workspace.FromContext(ctx)
	if !selected {
		return ctx, actor, nil
	}
	var owner string
	err := r.db.Pool().QueryRow(ctx, `SELECT account_id FROM paperboat.preview_leases WHERE id=$3 AND paperboat.workspace_preview_management_allowed($1,$2,id)`, actor, sql.NullString{String: scope.TeamID, Valid: scope.TeamID != ""}, preview).Scan(&owner)
	if isSolePreviewDomainError(err, pgx.ErrNoRows) {
		return ctx, "", ErrNotFound
	}
	return context.WithValue(ctx, workspaceManagementActorKey{}, actor), owner, err
}
func (s *Service) scopeWorkspacePreview(ctx context.Context, request previewtunnelapi.RequestContext, preview string) (context.Context, previewtunnelapi.RequestContext, error) {
	if resolver, ok := s.repository.(interface {
		ScopeWorkspacePreview(context.Context, string, string) (context.Context, string, error)
	}); ok {
		scoped, owner, err := resolver.ScopeWorkspacePreview(ctx, request.Actor.AccountID, preview)
		request.Actor.AccountID = owner
		return scoped, request, err
	}
	return ctx, request, nil
}
func authorizeWorkspacePreviewTx(ctx context.Context, tx *db.Tx, preview string) error {
	scope, selected := workspace.FromContext(ctx)
	if !selected {
		return nil
	}
	actor, _ := ctx.Value(workspaceManagementActorKey{}).(string)
	if actor == "" {
		return ErrOwnerDenied
	}
	if err := teams.Lock(ctx, tx); err != nil {
		return err
	}
	var allowed bool
	if err := tx.QueryRow(ctx, `SELECT paperboat.workspace_preview_management_allowed($1,$2,$3)`, actor, sql.NullString{String: scope.TeamID, Valid: scope.TeamID != ""}, preview).Scan(&allowed); err != nil {
		return err
	}
	if !allowed {
		return ErrNotFound
	}
	return nil
}
