package machineevents

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

// A busy account's hosts were answered about 4x more often than its topics moved (prod 2026-11-26). This checks
// that folding bumps into the base row while a host waits on the generation it holds is never answered as a move.
func TestFoldingBumpsIsNotAnsweredAsAChangeOnPostgres(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("requires isolated PostgreSQL")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	database, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err = db.Migrate(context.Background(), database); err != nil {
		t.Fatal(err)
	}
	account := fmt.Sprintf("acct_fold_%d", time.Now().UnixNano())
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := database.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatalf("%v: %s", err, query)
		}
	}
	exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@example.test','active')`, account)
	t.Cleanup(func() {
		_, _ = database.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.users WHERE id=$1`, account)
	})
	exec(`INSERT INTO paperboat.user_machines (id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state,last_seen_at)
VALUES($1||'_m',$1,$1||'_e','m','linux','amd64','/w','online','occupied',now())`, account)
	for range 40 {
		exec(`SELECT paperboat.publish_account_topic(ARRAY[$1::text], 'private_routes')`, account)
	}
	hub := NewHub(database.Pool(), database.Direct())
	go func() {
		for ctx.Err() == nil {
			_ = hub.ListenFor(ctx, 10*time.Second)
		}
	}()
	waitListening(t, ctx, database)
	machine := account + "_m"
	known, err := hub.Wait(ctx, machine, map[string]int64{TopicPrivateRoutes: 0}, time.Second)
	if err != nil || known[TopicPrivateRoutes] == 0 {
		t.Fatalf("first wait: %v %v", known, err)
	}
	// Folds run while the waiter re-reads on every account notification another account's publish cannot send,
	// so a notification for this account with no new bump forces the re-read under each fold.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 40 && ctx.Err() == nil; i++ {
			exec(`SELECT paperboat.fold_account_topic_bumps(1)`)
			exec(`SELECT pg_notify('machine_events', $1)`, accountKey(account))
		}
	}()
	changed, err := hub.Wait(ctx, machine, known, 5*time.Second)
	<-done
	if err != nil {
		t.Fatal(err)
	}
	if len(changed) != 0 {
		t.Fatalf("folding bumps answered a wait without a move: known %v, answered %v", known, changed)
	}
}
