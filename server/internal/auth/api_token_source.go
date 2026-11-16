package auth

import (
	"context"
	"database/sql"

	"github.com/pinksaucepasta/paperboat/server/internal/workspace"
)

// AuthorizeAPITokenSession binds a token request to its live peer endpoint and exact action.
// Creator access remains the caller's separate machine/workspace policy check.
func AuthorizeAPITokenSession(ctx context.Context, database interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, account, session, machine, capability string, port int) error {
	token, ok := APITokenFromContext(ctx)
	if !ok {
		return nil
	}
	scope, selected := workspace.FromContext(ctx)
	if !selected {
		return ErrOutsideTokenScope
	}
	var allowed bool
	err := database.QueryRowContext(ctx, `SELECT EXISTS(
 SELECT 1 FROM paperboat.cli_client_sessions c JOIN paperboat.api_tokens t ON t.id=c.api_token_id
 WHERE c.id=$1 AND c.user_id=$2 AND c.state='active' AND c.revoked_at IS NULL
 AND t.id=$3 AND t.account_id=$2 AND t.team_id IS NOT DISTINCT FROM nullif($4,'')
 AND paperboat.api_token_scope_allows(t.id,$5,$6,nullif($7,0)))`,
		session, account, token, scope.TeamID, machine, capability, port).Scan(&allowed)
	if err != nil {
		return err
	}
	if !allowed {
		return ErrOutsideTokenScope
	}
	return nil
}

// SDKSessionFromContext returns the API token or recipe a request's session runs under: both act
// as their creator and share the SDK limits and operation bookkeeping.
func SDKSessionFromContext(ctx context.Context) (string, bool) {
	if token, ok := APITokenFromContext(ctx); ok {
		return token, true
	}
	return RecipeFromContext(ctx)
}
