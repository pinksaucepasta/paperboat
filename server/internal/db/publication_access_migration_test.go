package db

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pressly/goose/v3"
)

// Migration 174 moves the preview and tunnel access rules into decision functions that take the host checks as
// arguments, so the lists and counts can evaluate each host check once per host. It is authorization SQL: every
// answer of the access functions (personal, workspace NULL and the team workspace) for every actor and
// publication of the fixture must be the same before and after it.
func TestPublicationAccessAnswersAreUnchangedByMigration174OnPostgres(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_MIGRATION_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("empty isolated migration PostgreSQL DSN required")
	}
	if err := ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	store, err := Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	var exists bool
	if err = store.SQL().QueryRowContext(ctx, `SELECT to_regclass('paperboat.users') IS NOT NULL`).Scan(&exists); err != nil || exists {
		t.Fatalf("requires empty isolated database: exists=%v error=%v", exists, err)
	}
	if _, err := store.SQL().ExecContext(ctx, `CREATE SCHEMA paperboat`); err != nil {
		t.Fatal(err)
	}
	goose.SetBaseFS(migrationsFS)
	goose.SetTableName("paperboat.goose_db_version")
	if err = goose.SetDialect("postgres"); err != nil {
		t.Fatal(err)
	}
	if err = goose.UpToContext(ctx, store.SQL(), migrationsDir, 173); err != nil {
		t.Fatal(err)
	}
	f := newPublicationFixture(t, ctx, store)
	answers := func() map[string]string {
		t.Helper()
		rows, err := store.SQL().QueryContext(ctx, `
SELECT 'preview '||a||' '||p.id, paperboat.preview_management_allowed(a,p.id)::text||
 paperboat.workspace_preview_management_allowed(a,NULL,p.id)::text||paperboat.workspace_preview_management_allowed(a,$2,p.id)::text
FROM unnest($1::text[]) a, paperboat.preview_leases p WHERE p.id LIKE 'pc_preview_%_'||$3
UNION ALL
SELECT 'tunnel '||a||' '||x.id, paperboat.tunnel_management_allowed(a,x.id)::text||
 paperboat.workspace_tunnel_management_allowed(a,NULL,x.id)::text||paperboat.workspace_tunnel_management_allowed(a,$2,x.id)::text
FROM unnest($1::text[]) a, paperboat.tunnels x WHERE x.id LIKE 'pc_tunnel_%_'||$3`, f.users, f.team, f.s)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		out := map[string]string{}
		for rows.Next() {
			var key, answer string
			if err := rows.Scan(&key, &answer); err != nil {
				t.Fatal(err)
			}
			out[key] = answer
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return out
	}
	before := answers()
	if err = goose.UpContext(ctx, store.SQL(), migrationsDir); err != nil {
		t.Fatal(err)
	}
	after := answers()
	allowed := 0
	for key, want := range before {
		if got := after[key]; got != want {
			t.Errorf("%s: %s before migration 174, %s after", key, want, got)
		}
		for _, c := range want {
			if c == 't' {
				allowed++
			}
		}
	}
	if len(after) != len(before) || len(before) < 200 || allowed < 40 {
		t.Fatalf("fixture too weak or changed: %d answers before, %d after, %d allowed", len(before), len(after), allowed)
	}
}
