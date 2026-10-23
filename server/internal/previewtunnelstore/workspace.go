package previewtunnelstore

import (
	"context"
	"database/sql"
	"errors"
	"github.com/jackc/pgx/v5"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/teams"
	"github.com/pinksaucepasta/paperboat/server/internal/workspace"
	"strings"
)

// ErrTeamSubscriptionRequired reports a publication which would otherwise be
// created successfully but denied by the existing team forwarding entitlement.
var ErrTeamSubscriptionRequired = errors.New("an active team subscription is required to publish in this workspace")

// RequireWorkspacePublisherEntitlementTx is called under the team authority
// lock, after membership and host authorization and before any creation writes.
func RequireWorkspacePublisherEntitlementTx(ctx context.Context, tx *db.Tx, team string) error {
	if team == "" {
		return nil
	}
	var entitled bool
	if err := tx.QueryRow(ctx, `SELECT paperboat.team_paid_entitled($1)`, team).Scan(&entitled); err != nil {
		return err
	}
	if !entitled {
		return ErrTeamSubscriptionRequired
	}
	return nil
}

func nullableText(value string) sql.NullString {
	return sql.NullString{String: value, Valid: value != ""}
}

// BindWorkspacePublisherTx records ownership's grant boundary in the same
// transaction as creation. It does not bind or grant access to the whole host.
func BindWorkspacePublisherTx(ctx context.Context, tx *db.Tx, team, kind, id, account string) error {
	var count int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM team_resource_bindings WHERE team_id=$1`, team).Scan(&count); err != nil {
		return err
	}
	if count >= teams.MaximumResources {
		return ErrInvalidInput
	}
	if _, err := tx.Exec(ctx, `INSERT INTO team_resource_bindings(team_id,resource_kind,resource_id,owner_account) VALUES($1,$2,$3,$4)`, team, kind, id, account); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `INSERT INTO team_resource_grants(team_id,account_id,resource_kind,resource_id,permission,generation,active) VALUES($1,$2,$3,$4,'use',1,true),($1,$2,$3,$4,'manage',1,true)`, team, account, kind, id)
	return err
}

func (s *Store) verifyWorkspacePreviewMachine(ctx context.Context, actor, machine, mode string) (bool, error) {
	ws, selected := workspace.FromContext(ctx)
	if !selected {
		return verifyPreviewMachineAuthority(s.db.Pool().QueryRow(ctx, previewMachineAuthoritySQL, actor, machine), mode)
	}
	return verifyPreviewMachineAuthority(s.db.Pool().QueryRow(ctx, `SELECT m.user_id<>$1 OR m.owner_team_id IS NOT NULL FROM paperboat.user_machines m WHERE m.id=$3 AND m.state='online' AND m.online AND m.deleted_at IS NULL AND m.revoked_at IS NULL AND m.configured_capabilities @> ARRAY['preview_launch']::text[] AND paperboat.workspace_publisher_host_capability_allowed($1,$2,m.id,'preview_manage')`, actor, nullableText(ws.TeamID), machine), mode)
}

// ResolveWorkspaceResourceAccount authorizes the exact selected resource before
// exposing its persistence account to operation/event and domain consumers.
func (s *Store) ResolveWorkspaceResourceAccount(ctx context.Context, actor, kind, id string) (string, error) {
	return resolveWorkspaceResourceAccount(ctx, s.db.Pool(), actor, kind, id)
}

type workspaceQuerier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func resolveWorkspaceResourceAccount(ctx context.Context, q workspaceQuerier, actor, kind, id string) (string, error) {
	ws, selected := workspace.FromContext(ctx)
	if !selected {
		return actor, nil
	}
	var query string
	switch kind {
	case "preview", "preview_lease":
		query = `SELECT account_id FROM paperboat.preview_leases WHERE id=$3 AND paperboat.workspace_preview_management_allowed($1,$2,id)`
	case "tunnel":
		query = `SELECT account_id FROM paperboat.tunnels WHERE id=$3 AND paperboat.workspace_tunnel_management_allowed($1,$2,id)`
	case "route":
		query = `SELECT t.account_id FROM paperboat.tunnel_routes r JOIN paperboat.tunnels t ON t.id=r.tunnel_id WHERE r.id=$3 AND paperboat.workspace_tunnel_management_allowed($1,$2,t.id)`
	case "domain_binding":
		query = `SELECT t.account_id FROM paperboat.tunnel_domains d JOIN paperboat.tunnels t ON t.id=d.tunnel_id WHERE d.id=$3 AND paperboat.workspace_tunnel_management_allowed($1,$2,t.id)`
	case "preview_domain_binding":
		query = `SELECT p.account_id FROM paperboat.preview_domains d JOIN paperboat.preview_leases p ON p.id=d.preview_id WHERE d.id=$3 AND paperboat.workspace_preview_management_allowed($1,$2,p.id)`
	case "connector":
		query = `SELECT t.account_id FROM paperboat.tunnel_connectors c JOIN paperboat.tunnels t ON t.id=c.tunnel_id WHERE c.id=$3 AND paperboat.workspace_tunnel_management_allowed($1,$2,t.id)`
	default:
		return "", ErrNotFound
	}
	var account string
	err := q.QueryRow(ctx, query, actor, nullableText(ws.TeamID), id).Scan(&account)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	return account, err
}

func (s *Store) authorizeWorkspacePreviewTx(ctx context.Context, tx *db.Tx, actor, preview string) error {
	ws, selected := workspace.FromContext(ctx)
	if !selected {
		return nil
	}
	if err := workspace.ValidateTx(ctx, tx, actor, ws); err != nil {
		return ErrNotFound
	}
	var allowed bool
	if err := tx.QueryRow(ctx, `SELECT paperboat.workspace_preview_management_allowed($1,$2,$3)`, actor, nullableText(ws.TeamID), preview).Scan(&allowed); err != nil {
		return err
	}
	if !allowed {
		return ErrNotFound
	}
	return nil
}

func resolveWorkspaceOperationAccount(ctx context.Context, q workspaceQuerier, actor, operation string) (string, error) {
	if _, selected := workspace.FromContext(ctx); !selected {
		return actor, nil
	}
	var kind, operationType string
	var resource sql.NullString
	if err := q.QueryRow(ctx, `SELECT resource_kind,resource_id,operation_type FROM paperboat.operations WHERE id=$1`, operation).Scan(&kind, &resource, &operationType); err != nil {
		return "", translate(err)
	}
	if !resource.Valid {
		return "", ErrNotFound
	}
	if kind == "domain_binding" && strings.HasPrefix(operationType, "preview.domain.") {
		kind = "preview_domain_binding"
	}
	return resolveWorkspaceResourceAccount(ctx, q, actor, kind, resource.String)
}
