package db

import (
	"context"
	"net/url"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pinksaucepasta/paperboat/server/internal/config"
)

// With startup_params off, the role defaults must give a new connection the same settings, and migrations must still
// run with no statement or lock timeout, as their pool always has.
func TestRoleDefaultsCarrySessionSettingsOnPostgres(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("requires isolated PostgreSQL")
	}
	if err := ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	// A statement_timeout in the DSN is a startup parameter and would beat the role default this test checks.
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	query := parsed.Query()
	query.Del("statement_timeout")
	parsed.RawQuery = query.Encode()
	dsn = parsed.String()
	ctx := context.Background()
	off := false
	store, err := Open(config.Database{Driver: "postgres", DSN: dsn, StartupParams: &off})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	// Role defaults belong to the role, not the database: clear them so the rest of the test cluster is unchanged.
	t.Cleanup(func() {
		for _, name := range sessionSettingNames {
			_, _ = store.SQL().ExecContext(context.Background(), "ALTER ROLE CURRENT_USER RESET "+name)
		}
	})
	if err := store.SetRoleDefaults(ctx, config.Database{Driver: "postgres", DSN: dsn}); err != nil {
		t.Fatal(err)
	}
	var searchPath, jit string
	if err := store.Pool().QueryRow(ctx, `SELECT current_setting('search_path'), current_setting('jit')`).Scan(&searchPath, &jit); err != nil {
		t.Fatal(err)
	}
	if searchPath != "paperboat" || jit != "off" {
		t.Fatalf("new connection search_path=%q jit=%q, want paperboat and off from the role defaults", searchPath, jit)
	}
	var statementTimeout string
	if err := store.Pool().QueryRow(ctx, `SELECT setting FROM pg_settings WHERE name='statement_timeout'`).Scan(&statementTimeout); err != nil || statementTimeout != "30000" {
		t.Fatalf("statement_timeout=%q err=%v, want 30000 from the role defaults", statementTimeout, err)
	}
	if err := Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	migrations, err := pgxpool.NewWithConfig(ctx, store.migrationConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer migrations.Close()
	var migrationTimeout string
	if err := migrations.QueryRow(ctx, `SELECT current_setting('statement_timeout')`).Scan(&migrationTimeout); err != nil || migrationTimeout != "0" {
		t.Fatalf("migration connection statement_timeout=%q err=%v, want 0", migrationTimeout, err)
	}
}
