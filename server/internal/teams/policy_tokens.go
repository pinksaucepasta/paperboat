package teams

import (
	"context"

	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/shared/policy"
)

// TokenScopeRowsTx checks an API token scope against the workspace's current policy (machine,
// tag, preview and tunnel names) and returns its enforcement rows for api_token_scope_rules.
func TokenScopeRowsTx(ctx context.Context, tx *db.Tx, w Workspace, scope []policy.ScopeEntry) ([]policy.Row, policy.Errors, error) {
	cur, err := policyTx(ctx, tx, w)
	if err != nil {
		return nil, nil, err
	}
	inv, err := policyInventoryTx(ctx, tx, w)
	if err != nil {
		return nil, nil, err
	}
	rows, errs := compileStored(cur.source, inv).ScopeRows(scope)
	return rows, errs, nil
}
