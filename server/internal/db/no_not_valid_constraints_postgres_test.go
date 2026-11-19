package db

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
)

// A constraint added NOT VALID is validated by the next migration (AGENTS.md, migrations on a live database);
// one nobody validates keeps the planner and the next reader guessing, so none may remain after Migrate.
func TestMigratedSchemaHasNoNotValidConstraintsOnPostgres(t *testing.T) {
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
	rows, err := store.SQL().QueryContext(ctx, `SELECT conrelid::regclass::text, conname FROM pg_constraint
 WHERE connamespace='paperboat'::regnamespace AND NOT convalidated ORDER BY 1, 2`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var table, name string
		if err := rows.Scan(&table, &name); err != nil {
			t.Fatal(err)
		}
		t.Errorf("%s: constraint %s is still NOT VALID; validate it in a later migration", table, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
}
