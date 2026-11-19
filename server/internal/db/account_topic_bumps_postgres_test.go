package db

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pinksaucepasta/paperboat/server/internal/config"
)

// A revoke returned 40001 to the user (CI 2026-11-19): every machine event of an account updated the same account topic
// row, and a serializable transaction that updates a row another transaction changed since its snapshot cannot commit,
// so the revoke lost that race to the account's enrollments on every retry. Here a serializable transaction takes its
// snapshot, another connection publishes the same account topic and commits, then the transaction publishes it too:
// both must commit, the topic's generation must grow by two, and folding the bumps must keep that total.
func TestAccountTopicPublishDoesNotConflictUnderSerializableOnPostgres(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("requires isolated PostgreSQL")
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
	t.Cleanup(func() { _ = store.Close() })
	if err := Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	account := fmt.Sprintf("topicbump_%d", time.Now().UnixNano())
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@example.test','active')`, account); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.users WHERE id=$1`, account)
	})
	generation := func() int64 {
		t.Helper()
		var g int64
		if err := store.SQL().QueryRowContext(ctx, `SELECT coalesce((SELECT generation FROM paperboat.account_topic_generations WHERE account_id=$1 AND topic='inventory'),0)`, account).Scan(&g); err != nil {
			t.Fatal(err)
		}
		return g
	}
	publish := `SELECT paperboat.publish_account_topic(ARRAY[$1::text], 'inventory')`
	if _, err := store.SQL().ExecContext(ctx, publish, account); err != nil { // the topic row exists, as for any active account
		t.Fatal(err)
	}
	before := generation()

	tx, err := store.Pool().BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, `SELECT 1`); err != nil { // takes the snapshot
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, publish, account); err != nil { // a concurrent machine event commits
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, publish, account); err != nil {
		t.Fatalf("publishing a topic another transaction just published failed: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if got := generation(); got != before+2 {
		t.Fatalf("generation %d after two publishes, want %d", got, before+2)
	}
	for {
		var folded int64
		if err := store.SQL().QueryRowContext(ctx, `SELECT paperboat.fold_account_topic_bumps(1000)`).Scan(&folded); err != nil {
			t.Fatal(err)
		}
		if folded < 1000 {
			break
		}
	}
	var bumps int
	if err := store.SQL().QueryRowContext(ctx, `SELECT count(*) FROM paperboat.account_topic_bumps WHERE account_id=$1`, account).Scan(&bumps); err != nil || bumps != 0 {
		t.Fatalf("%d bumps left after folding (%v)", bumps, err)
	}
	if got := generation(); got != before+2 {
		t.Fatalf("generation %d after folding, want %d", got, before+2)
	}
}
