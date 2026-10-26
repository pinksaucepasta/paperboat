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
	teamA := "fence-a"
	teamB := ""
	for i := 0; teamB == ""; i++ {
		if candidate := fmt.Sprintf("fence-b-%d", i); db.TeamWriteFenceBucket(candidate) != db.TeamWriteFenceBucket(teamA) {
			teamB = candidate
		}
	}
	held, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		done <- store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
			if err := db.LockTeamWritesFor(ctx, tx, teamA); err != nil {
				return err
			}
			close(held)
			<-release
			return nil
		})
	}()
	<-held
	// try runs one transaction that must take its lock within 500 ms (lock_timeout).
	try := func(lock func(context.Context, *db.Tx) error) error {
		return store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
			if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout = '500ms'`); err != nil {
				return err
			}
			return lock(ctx, tx)
		})
	}
	start := time.Now()
	if err := try(func(ctx context.Context, tx *db.Tx) error { return db.LockTeamWritesFor(ctx, tx, teamB) }); err != nil || time.Since(start) > 400*time.Millisecond {
		t.Fatalf("another team's write waited for team A: %v after %v", err, time.Since(start))
	}
	if err := try(func(ctx context.Context, tx *db.Tx) error { return db.LockTeamWritesFor(ctx, tx, teamA) }); err == nil {
		t.Fatal("a second write for team A did not wait for the first")
	}
	if err := try(db.LockTeamWrites); err == nil {
		t.Fatal("a global team write did not wait for a team's write")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
