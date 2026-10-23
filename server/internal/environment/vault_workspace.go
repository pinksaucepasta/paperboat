package environment

import (
	"context"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

// Private member scopes use the member's Personal key, but membership and Team
// ENV authority still gate their workspace coordinate. Team roles grant no access
// to another member's private account or device scopes.
func authorizeVaultScopeWorkspaceTx(ctx context.Context, tx *db.Tx, account, workspace, kind, owner, machine string) (uint64, error) {
	if !validIdentifier(workspace) || kind == "team" && workspace != owner {
		return 0, ErrInvalidScope
	}
	epoch, err := authorizeVaultScopeTx(ctx, tx, account, kind, owner, machine)
	if err != nil {
		return 0, err
	}
	if kind == "personal" && workspace != "personal" {
		if err := requireVaultWorkspaceMemberTx(ctx, tx, account, workspace); err != nil {
			return 0, err
		}
	}

	return epoch, nil
}

func invalidateVaultLayerSourceTx(ctx context.Context, tx *db.Tx, workspace, kind, owner, machine string) error {
	_, err := tx.Exec(ctx, `INSERT INTO environment_vault_layer_fences(workspace_id,owner_kind,owner_id,source_machine_id,generation) VALUES($1,$2,$3,$4,1) ON CONFLICT(workspace_id,owner_kind,owner_id,source_machine_id) DO UPDATE SET generation=environment_vault_layer_fences.generation+1`, workspace, kind, owner, machine)
	return err
}

func scopeDocumentKey(workspace, machine string) string { return workspace + "\x00" + machine }
