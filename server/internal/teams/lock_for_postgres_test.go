package teams

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

// Team-scoped writers (billing, SMTP, audit stream, config defaults, workspace previews) took the server-wide
// fence, so one team's write queued every other team's. LockFor locks only the team's bucket; "" stays global.
func TestLockForSerializesOnlyTheSameTeamOnPostgres(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("requires isolated PostgreSQL")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	store, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := db.Migrate(context.Background(), store); err != nil {
		t.Fatal(err)
	}
	teamA, teamB := "lockfor-a", ""
	for i := 0; teamB == ""; i++ {
		if c := fmt.Sprintf("lockfor-b-%d", i); db.TeamWriteFenceBucket(c) != db.TeamWriteFenceBucket(teamA) {
			teamB = c
		}
	}
	try := func(team string) error {
		return store.InTx(context.Background(), func(ctx context.Context, tx *db.Tx) error {
			if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout = '500ms'`); err != nil {
				return err
			}
			return LockFor(ctx, tx, team)
		})
	}
	held, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		done <- store.InTx(context.Background(), func(ctx context.Context, tx *db.Tx) error {
			if err := LockFor(ctx, tx, teamA); err != nil {
				return err
			}
			close(held)
			<-release
			return nil
		})
	}()
	<-held
	start := time.Now()
	if err := try(teamB); err != nil || time.Since(start) > 400*time.Millisecond {
		t.Errorf("another team's write waited for team A: %v after %v", err, time.Since(start))
	}
	if err := try(teamA); err == nil {
		t.Error("a second write for team A did not wait")
	}
	if err := try(""); err == nil {
		t.Error("an unscoped write did not wait for team A")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
