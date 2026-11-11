package operatorauth

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

// F21-157: the first-operator bootstrap replays its serializable transaction after one 40001
// instead of answering an error, and still consumes the bootstrap exactly once.
func TestSetupSurvivesOneSerializationFailureOnPostgres(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("requires isolated remote PostgreSQL")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	store, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	ctx := context.Background()
	if err = db.Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	// Isolated database: the singleton bootstrap row may exist from another test in this package.
	for _, q := range []string{
		`DELETE FROM paperboat.operator_sessions`, `DELETE FROM paperboat.operator_bootstrap`, `DELETE FROM paperboat.operator_accounts WHERE email LIKE 'setup-retry-%'`,
	} {
		if _, err = store.SQL().ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	suffix := time.Now().UnixNano()
	seq := fmt.Sprintf("setup_retry_seq_%d", suffix)
	fn := fmt.Sprintf("setup_retry_fn_%d", suffix)
	email := fmt.Sprintf("setup-retry-%d@example.test", suffix)
	t.Cleanup(func() {
		bg := context.Background()
		store.SQL().ExecContext(bg, fmt.Sprintf(`DROP TRIGGER IF EXISTS %s ON paperboat.operator_sessions`, fn))
		store.SQL().ExecContext(bg, fmt.Sprintf(`DROP FUNCTION IF EXISTS paperboat.%s()`, fn))
		store.SQL().ExecContext(bg, fmt.Sprintf(`DROP SEQUENCE IF EXISTS paperboat.%s`, seq))
		store.SQL().ExecContext(bg, `DELETE FROM paperboat.operator_sessions`)
		store.SQL().ExecContext(bg, `DELETE FROM paperboat.operator_bootstrap`)
		store.SQL().ExecContext(bg, `DELETE FROM paperboat.operator_accounts WHERE email=$1`, email)
	})
	for _, q := range []string{
		fmt.Sprintf(`CREATE SEQUENCE paperboat.%s`, seq),
		fmt.Sprintf(`CREATE FUNCTION paperboat.%s() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF nextval('paperboat.%s') = 1 THEN RAISE EXCEPTION 'forced' USING ERRCODE = '40001'; END IF; RETURN NEW; END $$`, fn, seq),
		fmt.Sprintf(`CREATE TRIGGER %s BEFORE INSERT ON paperboat.operator_sessions FOR EACH ROW EXECUTE FUNCTION paperboat.%s()`, fn, fn),
	} {
		if _, err = store.SQL().ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	s := &Service{Store: store, BootstrapEmail: email}
	session, err := s.Setup(ctx, email, "a sufficiently long operator password")
	if err != nil {
		t.Fatalf("Setup after one forced 40001: %v", err)
	}
	if session.Account.Email != email {
		t.Fatalf("session account %q", session.Account.Email)
	}
	var bootstraps, calls int
	if err = store.SQL().QueryRowContext(ctx, `SELECT count(*) FROM paperboat.operator_bootstrap`).Scan(&bootstraps); err != nil || bootstraps != 1 {
		t.Fatalf("bootstrap rows %d %v", bootstraps, err)
	}
	if err = store.SQL().QueryRowContext(ctx, fmt.Sprintf(`SELECT last_value FROM paperboat.%s`, seq)).Scan(&calls); err != nil || calls != 2 {
		t.Fatalf("the insert ran %d times, want 2 (one forced failure, one replay): %v", calls, err)
	}
	if _, err = s.Setup(ctx, email, "a sufficiently long operator password"); err != ErrBootstrapConsumed {
		t.Fatalf("second Setup: %v", err)
	}
}
