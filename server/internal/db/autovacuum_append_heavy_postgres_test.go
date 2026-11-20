package db

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
)

// Append-heavy tables with a few thousand rows a day never reached the default 0.2 insert / 0.1 analyze scale factors
// (audit_events at 366k rows, resource_bandwidth_events at 631k rows: statistics 25x stale), so each carries its own.
func TestAppendHeavyTablesCarryAutovacuumScaleFactorsOnPostgres(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("requires isolated PostgreSQL")
	}
	if err := ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	store, err := Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"audit_events", "resource_bandwidth_events"} {
		var options string
		if err := store.SQL().QueryRowContext(ctx, `SELECT coalesce(array_to_string(reloptions, ','), '') FROM pg_class WHERE oid = ('paperboat.'||$1)::regclass`, table).Scan(&options); err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"autovacuum_vacuum_insert_scale_factor=0.02", "autovacuum_analyze_scale_factor=0.02", "autovacuum_vacuum_scale_factor=0.05"} {
			if !strings.Contains(options, want) {
				t.Errorf("%s reloptions %q lack %s", table, options, want)
			}
		}
	}
}
