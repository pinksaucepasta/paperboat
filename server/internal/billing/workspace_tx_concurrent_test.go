package billing_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/billing"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

// Reading a workspace that exists must not write to it: an INSERT .. ON CONFLICT DO NOTHING over a row another
// transaction updated after this one's snapshot (the usage writers do, constantly) is a 40001.
func TestWorkspaceTxIgnoresAConcurrentUpdateOfTheExistingRowPostgres(t *testing.T) {
	store := testStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	user := fmt.Sprintf("usr_wstx_%d", time.Now().UnixNano())
	personal := "personal:" + user
	insertUser(t, store, user, user+"@example.test")
	t.Cleanup(func() {
		_, _ = store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.billing_workspaces WHERE id=$1`, personal)
		_, _ = store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.users WHERE id=$1`, user)
	})
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.billing_workspaces(id,user_id,team_id,state) VALUES($1,$2,NULL,'active')`, personal, user); err != nil {
		t.Fatal(err)
	}
	before := store.TransactionStats()
	snapshot, bumped := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		attempt := 0
		done <- store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
			attempt++
			var one int
			if err := tx.QueryRow(ctx, `SELECT 1 FROM billing_workspaces WHERE id=$1`, personal).Scan(&one); err != nil {
				return err
			}
			if attempt == 1 {
				close(snapshot)
				<-bumped
			}
			_, err := billing.WorkspaceTx(ctx, tx, user, "", false)
			return err
		})
	}()
	<-snapshot
	if _, err := store.SQL().ExecContext(ctx, `UPDATE paperboat.billing_workspaces SET revision=revision+1 WHERE id=$1`, personal); err != nil {
		t.Fatal(err)
	}
	close(bumped)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if n := store.TransactionStats().Serializable.Retries - before.Serializable.Retries; n != 0 {
		t.Fatalf("reading an existing workspace retried %d transaction attempts (40001)", n)
	}
}
