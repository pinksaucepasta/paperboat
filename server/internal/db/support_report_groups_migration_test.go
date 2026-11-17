package db

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pressly/goose/v3"
)

// 386-387: support_reports rows still alive become occurrences and groups: the same failure of one account is one
// group with its count and first/last seen, another account's identical failure is its own group, and an expired row
// is not carried over.
func TestSupportReportGroupsBackfill(t *testing.T) {
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
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	var exists bool
	if err = store.SQL().QueryRowContext(ctx, `SELECT to_regclass('paperboat.users') IS NOT NULL`).Scan(&exists); err != nil || exists {
		t.Fatalf("requires empty isolated database: exists=%v error=%v", exists, err)
	}
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`CREATE SCHEMA paperboat`)
	goose.SetBaseFS(migrationsFS)
	goose.SetTableName("paperboat.goose_db_version")
	if err = goose.SetDialect("postgres"); err != nil {
		t.Fatal(err)
	}
	if err = goose.UpToContext(ctx, store.SQL(), migrationsDir, 385); err != nil {
		t.Fatal(err)
	}
	report := func(id, owner, ref string, line int, age, expires string) {
		exec(`INSERT INTO paperboat.support_reports(id,support_reference,owner_id,request_id,component,release,stage,code,created_at,expires_at,sentry_event_id,source_file,source_function,source_line,reporting,operation,correlation_id,cause,error_type,error_chain,errno,http_status,outcome,severity)
VALUES($1,$2,$3,'request_'||$1,'paperboat-server','dev','http_handler','internal_error',now()-$5::interval,now()+$6::interval,'','f.go','f',$4,'disabled','http_other','','internal','none','{}',0,500,'failed','error')`,
			id, "support_"+ref+"-0000-4000-8000-000000000000", owner, line, age, expires)
	}
	report("r1", "acct_a", "11111111", 7, "3 days", "4 days")
	report("r2", "acct_a", "22222222", 7, "1 day", "6 days")
	report("r3", "acct_a", "33333333", 7, "1 hour", "7 days")
	report("r4", "acct_a", "44444444", 8, "1 hour", "7 days")
	report("r5", "acct_b", "55555555", 7, "1 hour", "7 days")
	report("old", "acct_a", "66666666", 7, "8 days", "-1 hour")
	if err = goose.UpToContext(ctx, store.SQL(), migrationsDir, 387); err != nil {
		t.Fatal(err)
	}
	var groups, occurrences, grouped int
	var first, last time.Time
	if err = store.SQL().QueryRowContext(ctx, `SELECT (SELECT count(*) FROM paperboat.support_report_groups),
 (SELECT count(*) FROM paperboat.support_report_occurrences),
 (SELECT occurrences FROM paperboat.support_report_groups WHERE owner_id='acct_a' AND source_line=7),
 (SELECT first_seen FROM paperboat.support_report_groups WHERE owner_id='acct_a' AND source_line=7),
 (SELECT last_seen FROM paperboat.support_report_groups WHERE owner_id='acct_a' AND source_line=7)`).Scan(&groups, &occurrences, &grouped, &first, &last); err != nil {
		t.Fatal(err)
	}
	if groups != 3 || occurrences != 5 || grouped != 3 || !last.After(first.Add(24*time.Hour)) {
		t.Fatalf("backfill: %d groups, %d occurrences, group of three counts %d, seen %v..%v; want 3, 5, 3", groups, occurrences, grouped, first, last)
	}
}
