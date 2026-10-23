package db

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
)

// Forty concurrent serializable transactions that take the team write lock and then read-modify-write the
// same row all commit (Lane W: 40 parallel exec descriptors for one machine answered 15 503s). With an
// advisory lock taken by a SELECT, each waiter's snapshot predated the lock and many exhausted their
// serialization retries; LOCK TABLE takes no snapshot.
func TestTeamWriteLockGivesEachWaiterAFreshSnapshotOnPostgres(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("isolated PostgreSQL required")
	}
	if err := ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	store, err := Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	if err = Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	table := fmt.Sprintf("team_fence_probe_%d", time.Now().UnixNano())
	if _, err = store.SQL().ExecContext(ctx, `CREATE TABLE paperboat.`+table+` (id int PRIMARY KEY, n int NOT NULL); INSERT INTO paperboat.`+table+` VALUES (1, 0)`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.SQL().ExecContext(context.Background(), `DROP TABLE paperboat.`+table) })
	before := store.TransactionStats().Serializable.Exhausted
	const writers = 40
	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- store.InTx(ctx, func(ctx context.Context, tx *Tx) error {
				if err := LockTeamWrites(ctx, tx); err != nil {
					return err
				}
				var n int
				if err := tx.QueryRow(ctx, `SELECT n FROM `+table+` WHERE id=1`).Scan(&n); err != nil {
					return err
				}
				_, err := tx.Exec(ctx, `UPDATE `+table+` SET n=$1 WHERE id=1`, n+1)
				return err
			})
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("a locked writer failed: %v", err)
		}
	}
	var n int
	if err := store.SQL().QueryRowContext(ctx, `SELECT n FROM paperboat.`+table+` WHERE id=1`).Scan(&n); err != nil || n != writers {
		t.Fatalf("counter = %d (%v), want %d: a writer read a stale snapshot", n, err, writers)
	}
	if exhausted := store.TransactionStats().Serializable.Exhausted - before; exhausted != 0 {
		t.Fatalf("%d transactions exhausted their serialization retries", exhausted)
	}
}
