package db

import (
	"context"
	"os"
	"testing"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
)

// Final machine deletion is one DELETE on user_machines that cascades. Any NO ACTION/RESTRICT FK that points at a
// table in that cascade tree makes the whole deletion fail for every machine with such a row (migration 212).
// A new FK into the tree must cascade or clear (SET NULL); a self-referencing chain is deleted in one statement.
func TestMachineDeletionCascadeTreeHasNoBlockingFKs(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("requires isolated PostgreSQL")
	}
	if err := ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	store, err := Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	if err := Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	rows, err := store.SQL().QueryContext(ctx, `WITH RECURSIVE tree(rel) AS (
  SELECT 'paperboat.user_machines'::regclass::oid
  UNION
  SELECT c.conrelid FROM pg_constraint c JOIN tree ON c.confrelid=tree.rel WHERE c.contype='f' AND c.confdeltype='c'
)
SELECT c.conrelid::regclass::text||'.'||c.conname FROM pg_constraint c
WHERE c.contype='f' AND c.confdeltype IN ('a','r','d') AND c.confrelid IN (SELECT rel FROM tree)
  AND c.conrelid<>c.confrelid ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		t.Errorf("%s blocks final machine deletion: make it ON DELETE CASCADE or SET NULL", name)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
}
