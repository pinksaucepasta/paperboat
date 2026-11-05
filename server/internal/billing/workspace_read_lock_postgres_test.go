package billing_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/billing"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/teams"
)

// A billing read must not take the global team-write lock: while one is open, a team write
// (membership, grants, seats) still gets the lock at once.
func TestPostgresBillingReadDoesNotBlockTeamWrites(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	x := fmt.Sprint(time.Now().UnixNano())
	owner, team := "usr_rl_"+x, "rl-"+x
	insertUser(t, store, owner, owner+"@example.test")
	for _, q := range []string{
		`INSERT INTO paperboat.teams(team_id,owner_account,generation,display_name) VALUES($1,$2,1,'Read lock')`,
		`INSERT INTO paperboat.team_members(team_id,account_id,membership_generation,role,active) VALUES($1,$2,1,'admin',true)`,
	} {
		if _, err := store.SQL().ExecContext(ctx, q, team, owner); err != nil {
			t.Fatal(err)
		}
	}
	read, release := make(chan error, 1), make(chan struct{})
	go func() {
		read <- store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
			if _, err := billing.WorkspaceTx(ctx, tx, owner, team, false); err != nil {
				return err
			}
			read <- nil
			<-release // the read transaction stays open while the write runs
			return nil
		})
	}()
	if err := <-read; err != nil {
		t.Fatal(err)
	}
	write := store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout = '2s'`); err != nil {
			return err
		}
		return teams.Lock(ctx, tx)
	})
	close(release)
	if write != nil {
		t.Fatalf("a team write waited for an open billing read: %v", write)
	}
	if err := <-read; err != nil {
		t.Fatal(err)
	}
}
