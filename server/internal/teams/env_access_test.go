package teams

import (
	"context"
	"testing"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

// setENVAccess sets a member's team ENV permission (read, write, or "" for none), as `pb team grant <team> <account> env <team> <permission>` does.
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
