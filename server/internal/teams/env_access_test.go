package teams

import (
	"context"
	"testing"

	"github.com/pinksaucepasta/paperboat-server/internal/config"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
)

// setENVAccess is teamstest.SetENVAccess for this package's tests (teamstest imports teams).
func setENVAccess(t *testing.T, store *db.DB, team, account, permission string) {
	t.Helper()
	err := store.InTx(context.Background(), func(ctx context.Context, tx *db.Tx) error {
		if err := Lock(ctx, tx); err != nil {
			return err
		}
		return ApplyENVAccessTx(ctx, tx, config.Default().WorkspacePolicy, team, "", account, permission)
	})
	if err != nil {
		t.Fatal(err)
	}
}
