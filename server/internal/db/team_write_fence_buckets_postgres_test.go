package db_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

// Two teams in different buckets write at the same time; the same team, or a global writer, waits.
func TestTeamWritesForDifferentTeamsDoNotSerializePostgres(t *testing.T) {
	store := openFenceStore(t)
	teamA := "fence-a"
	teamB := ""
	for i := 0; teamB == ""; i++ {
		if candidate := fmt.Sprintf("fence-b-%d", i); db.TeamWriteFenceBucket(candidate) != db.TeamWriteFenceBucket(teamA) {
			teamB = candidate
		}
	}
	release := holdFence(t, store, func(ctx context.Context, tx *db.Tx) error { return db.LockTeamWritesFor(ctx, tx, teamA) })
	start := time.Now()
	if err := tryFence(store, func(ctx context.Context, tx *db.Tx) error { return db.LockTeamWritesFor(ctx, tx, teamB) }); err != nil || time.Since(start) > 400*time.Millisecond {
		t.Fatalf("another team's write waited for team A: %v after %v", err, time.Since(start))
	}
	if err := tryFence(store, func(ctx context.Context, tx *db.Tx) error { return db.LockTeamWritesFor(ctx, tx, teamA) }); err == nil {
		t.Fatal("a second write for team A did not wait for the first")
	}
	if err := tryFence(store, db.LockTeamWrites); err == nil {
		t.Fatal("a global team write did not wait for a team's write")
	}
	release()
}

func openFenceStore(t *testing.T) *db.DB {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("set PAPERBOAT_TEST_DATABASE_DSN")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	store, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	if err := db.Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	return store
}

// holdFence holds lock in one transaction until the returned release is called.
func holdFence(t *testing.T, store *db.DB, lock func(context.Context, *db.Tx) error) func() {
	held, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		done <- store.InTx(context.Background(), func(ctx context.Context, tx *db.Tx) error {
			if err := lock(ctx, tx); err != nil {
				return err
			}
			close(held)
			<-release
			return nil
		})
	}()
	<-held
	return func() {
		close(release)
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
}

// tryFence runs one transaction that must take its lock within 500 ms (lock_timeout).
func tryFence(store *db.DB, lock func(context.Context, *db.Tx) error) error {
	return store.InTx(context.Background(), func(ctx context.Context, tx *db.Tx) error {
		if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout = '500ms'`); err != nil {
			return err
		}
		return lock(ctx, tx)
	})
}

// Team readers (config status reports) never wait for each other, even in the same bucket, but do wait for the
// team's writers and the global lock, and the team's writers wait for them; writes outside teams wait only for
// the global lock (FIX-TEAMLOCK).
func TestTeamReadsDoNotSerializePostgres(t *testing.T) {
	store := openFenceStore(t)
	teamA := "fence-read-a"
	sameBucket := ""
	for i := 0; sameBucket == ""; i++ {
		if candidate := fmt.Sprintf("fence-read-b-%d", i); db.TeamWriteFenceBucket(candidate) == db.TeamWriteFenceBucket(teamA) {
			sameBucket = candidate
		}
	}
	readsFor := func(team string) func(context.Context, *db.Tx) error {
		return func(ctx context.Context, tx *db.Tx) error { return db.LockTeamReadsFor(ctx, tx, team) }
	}
	writesFor := func(team string) func(context.Context, *db.Tx) error {
		return func(ctx context.Context, tx *db.Tx) error { return db.LockTeamWritesFor(ctx, tx, team) }
	}
	release := holdFence(t, store, readsFor(teamA))
	for _, team := range []string{teamA, sameBucket} {
		if err := tryFence(store, readsFor(team)); err != nil {
			t.Fatalf("a read for %s waited for a read of %s: %v", team, teamA, err)
		}
	}
	if err := tryFence(store, writesFor(teamA)); err == nil {
		t.Fatal("a team write did not wait for the team's read")
	}
	if err := tryFence(store, db.LockTeamWrites); err == nil {
		t.Fatal("a global team write did not wait for a team's read")
	}
	if err := tryFence(store, lockNoTeams); err != nil {
		t.Fatalf("a write outside teams waited for a team read: %v", err)
	}
	release()
	release = holdFence(t, store, writesFor(teamA))
	if err := tryFence(store, lockNoTeams); err != nil {
		t.Fatalf("a write outside teams waited for a team write: %v", err)
	}
	if err := tryFence(store, readsFor(teamA)); err == nil {
		t.Fatal("a team read did not wait for the team's write")
	}
	release()
	release = holdFence(t, store, db.LockTeamWrites)
	for name, lock := range map[string]func(context.Context, *db.Tx) error{"team read": readsFor(teamA), "write outside teams": lockNoTeams} {
		if err := tryFence(store, lock); err == nil {
			t.Fatalf("a %s did not wait for the global write", name)
		}
	}
	release()
}

// lockNoTeams is how a transaction that touches no team's state orders itself against team writes.
func lockNoTeams(ctx context.Context, tx *db.Tx) error { return db.LockTeamsFor(ctx, tx, true) }
