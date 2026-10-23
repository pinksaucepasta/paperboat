package db_test

import (
	"context"
	"os"
	"testing"

	"github.com/pinksaucepasta/paperboat-server/internal/config"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
)

// Since the workspace policy (PLAN F18, migrations 128/139/140) owns team machine access, the
// private-access switch, team ENV and inspect/replay, the old tables are written by nobody:
// no SQL function or view may still read team_machine_grants or teams.allow_private_access
// (a reader would silently see no access).
func TestNoSQLReadsThePolicyTablesItReplaced(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("isolated PostgreSQL DSN required")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	store, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if err := db.Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	rows, err := store.SQL().QueryContext(ctx, `
	  SELECT 'function '||p.proname FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace
	  WHERE n.nspname='paperboat' AND p.prokind='f'
	    AND (pg_get_functiondef(p.oid) ~ 'team_machine_grants' OR pg_get_functiondef(p.oid) ~ '[^a-z_]t\.allow_private_access|teams\.allow_private_access')
	  UNION ALL
	  SELECT 'view '||viewname FROM pg_views WHERE schemaname='paperboat'
	    AND (definition ~ 'team_machine_grants' OR definition ~ 'teams\.allow_private_access')`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var reader string
		if err := rows.Scan(&reader); err != nil {
			t.Fatal(err)
		}
		t.Errorf("%s still reads a table the workspace policy replaced", reader)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
}
