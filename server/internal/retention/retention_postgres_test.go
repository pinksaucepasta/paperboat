package retention

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

// Nothing was ever deleted from the tables that grow with traffic. One pass deletes a whole backlog past its
// window in bounded batches, keeps what is inside it, and caps per-account history; every
// policy's statement runs on the migrated schema.
func TestRetentionDrainsBacklogsAndKeepsLiveRowsOnPostgres(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("requires isolated PostgreSQL")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	store, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := db.Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	s := fmt.Sprint(time.Now().UnixNano())
	user := "ret_user_" + s
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatalf("%v\n%s", err, query)
		}
	}
	count := func(query string, args ...any) int {
		t.Helper()
		var n int
		if err := store.SQL().QueryRowContext(ctx, query, args...).Scan(&n); err != nil {
			t.Fatalf("%v\n%s", err, query)
		}
		return n
	}
	exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@example.test','active')`, user)
	// 5,000 idempotency records two days old (past the 24 h window) and 10 fresh ones.
	exec(`INSERT INTO paperboat.managed_ssh_operations(operation_id,user_id,operation_kind,request_hash,resource_id,result_revision,created_at)
SELECT 'retention-op-'||$2||'-'||g,$1,'target_update',sha256(convert_to(g::text,'UTF8')),'r'||g,1,CASE WHEN g<=5000 THEN now()-interval '2 days' ELSE now() END
FROM generate_series(1,5010) g`, user, s)
	// 1,050 support report occurrences for one account (cap 1,000) and 3 expired ones, in a live group and a dead one.
	exec(`INSERT INTO paperboat.support_report_groups(owner_id,fingerprint,component,operation,stage,code,cause,error_type,http_status,outcome,
source_file,source_function,source_line,severity,errno,first_seen,last_seen,occurrences,expires_at)
VALUES ($1,'live','paperboat-server','op','http_handler','internal_error','none','none',500,'failed','f.go','f',1,'error',0,now(),now(),1050,now()+interval '7 days'),
($1,'dead','paperboat-server','op','http_handler','internal_error','none','none',500,'failed','f.go','f',2,'error',0,now()-interval '8 days',now()-interval '1 minute',3,now()-interval '1 minute')`, user)
	exec(`INSERT INTO paperboat.support_report_occurrences(id,owner_id,support_reference,fingerprint,request_id,correlation_id,release,created_at,expires_at,sentry_event_id,reporting)
SELECT 'ret_report_'||$2||'_'||g,$1,'support_'||gen_random_uuid(),CASE WHEN g>1050 THEN 'dead' ELSE 'live' END,'request_'||g,'','dev',
 now()-make_interval(secs=>g),CASE WHEN g>1050 THEN now()-interval '1 minute' ELSE now()+interval '7 days' END,'','disabled'
FROM generate_series(1,1053) g`, user, s)

	cfg := config.Default().Retention
	result, err := Run(ctx, store, cfg)
	if err != nil {
		t.Fatal(err) // every policy's statement ran
	}
	if n := count(`SELECT count(*) FROM paperboat.managed_ssh_operations WHERE user_id=$1`, user); n != 10 || result["managed_ssh_operations"] < 5000 {
		t.Fatalf("managed_ssh_operations: %d left (want the 10 fresh), %d deleted", n, result["managed_ssh_operations"])
	}
	if n := count(`SELECT count(*) FROM paperboat.support_report_occurrences WHERE owner_id=$1`, user); n != cfg.SupportReportsPerAccount {
		t.Fatalf("support_reports: %d left for the account, want its newest %d", n, cfg.SupportReportsPerAccount)
	}
	if n := count(`SELECT count(*) FROM paperboat.support_report_occurrences WHERE owner_id=$1 AND expires_at<now()`, user); n != 0 {
		t.Fatalf("%d expired support reports kept", n)
	}
	if n := count(`SELECT count(*) FROM paperboat.support_report_groups WHERE owner_id=$1 AND fingerprint='dead'`, user); n != 0 {
		t.Fatal("a support report group with no live occurrence was kept")
	}
	if n := count(`SELECT count(*) FROM paperboat.support_report_groups WHERE owner_id=$1 AND fingerprint='live'`, user); n != 1 {
		t.Fatal("the live support report group was deleted")
	}
	if newest := count(`SELECT count(*) FROM paperboat.support_report_occurrences WHERE owner_id=$1 AND id='ret_report_'||$2||'_1'`, user, s); newest != 1 {
		t.Fatal("the account's newest report was deleted")
	}
	// A second pass finds nothing more.
	again, err := Run(ctx, store, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if again["managed_ssh_operations"] != 0 || again["support_reports_per_account"] != 0 {
		t.Fatalf("second pass deleted again: %v", again)
	}
}

// A deleted team's people, invitations and settings go once the window passes; its row, its billing record and a team
// deleted more recently stay. The table list is checked against the schema, so a new team table must be classified here.
func TestDeletedTeamsAreTrimmedAfterTheirWindowOnPostgres(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("requires isolated PostgreSQL")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	store, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := db.Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	// Every table that names a team is purged here, removed with the team, cascaded from a purged table, or has its own window.
	classified := map[string]string{}
	for _, table := range deletedTeamTables {
		classified[table] = "purged"
	}
	for table, why := range map[string]string{
		"teams": "the tombstone: the name stays so history reads", "billing_workspaces": "billing record", "billing_team_orders": "billing record",
		"team_resource_grants": "cascades from team_members", "team_terminal_session_grants": "cascades from team_members", "team_machine_grants": "cascades from team_members",
		"team_config_default_adoptions": "cascades from team_config_defaults",
		"environment_vault_teams":       "removed at deletion", "environment_vault_team_grants": "removed at deletion", "environment_vault_team_members": "removed at deletion",
		"recording_keys": "removed at deletion", "recording_key_grants": "removed at deletion", "session_recordings": "removed at deletion; Recordings window",
		"team_inbox_requests": "InboxRequests window", "browser_access_sessions": "EndedSessions window", "browser_access_transactions": "EndedSessions window",
		"browser_machine_credentials": "ExpiredCredentials window", "inspector_credentials": "ExpiredCredentials window", "user_machine_access_sessions": "EndedSessions window",
		"user_machine_terminal_sessions": "EndedSessions window", "user_machines": "final-deleted once revoked", "endpoints": "EndedPublications window", "tunnels": "EndedPublications window",
		"preview_leases": "EndedPublications window", "workspace_domains": "EndedPublications window", "machine_port_names": "cascades with its machine",
		"api_tokens": "revoked at deletion; owned by an account", "recipes": "owned by an account", "environment_named_vaults": "owned by an account's vault",
	} {
		classified[table] = why
	}
	rows, err := store.SQL().QueryContext(ctx, `SELECT DISTINCT table_name FROM information_schema.columns WHERE table_schema='paperboat' AND column_name IN ('team_id','owner_team_id') AND table_name IN (SELECT table_name FROM information_schema.tables WHERE table_schema='paperboat' AND table_type='BASE TABLE')`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var table string
		if err := rows.Scan(&table); err != nil {
			t.Fatal(err)
		}
		if classified[table] == "" {
			t.Errorf("table %s names a team: add it to deletedTeamTables or say here why it stays", table)
		}
	}
	rows.Close()

	s := fmt.Sprint(time.Now().UnixNano())
	user, old, recent := "dt_user_"+s, "dt-old-"+s, "dt-recent-"+s
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatalf("%v\n%s", err, query)
		}
	}
	count := func(query string, args ...any) int {
		t.Helper()
		var n int
		if err := store.SQL().QueryRowContext(ctx, query, args...).Scan(&n); err != nil {
			t.Fatalf("%v\n%s", err, query)
		}
		return n
	}
	exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@example.test','active')`, user)
	for team, age := range map[string]string{old: "40 days", recent: "5 days"} {
		exec(`INSERT INTO paperboat.teams(team_id,owner_account,generation,deleted_at) VALUES($1,$2,1,now()-$3::interval)`, team, user, age)
		exec(`INSERT INTO paperboat.team_members(team_id,account_id,membership_generation,role,active) VALUES($1,$2,1,'member',false)`, team, user)
		exec(`INSERT INTO paperboat.team_invitations(invitation_id,team_id,recipient_account,created_by,expires_at,cancelled_at) VALUES('inv_'||$1,$1,$2,$2,now(),now())`, team, user)
		exec(`INSERT INTO paperboat.team_network_preferences(team_id,revision) VALUES($1,1)`, team)
	}
	t.Cleanup(func() {
		bg := context.Background()
		for _, team := range []string{old, recent} {
			store.SQL().ExecContext(bg, `DELETE FROM paperboat.team_members WHERE team_id=$1`, team)
			store.SQL().ExecContext(bg, `DELETE FROM paperboat.team_invitations WHERE team_id=$1`, team)
			store.SQL().ExecContext(bg, `DELETE FROM paperboat.team_network_preferences WHERE team_id=$1`, team)
			store.SQL().ExecContext(bg, `DELETE FROM paperboat.teams WHERE team_id=$1`, team)
		}
	})
	cfg := config.Default().Retention
	if _, err = Run(ctx, store, cfg); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{`SELECT count(*) FROM paperboat.team_members WHERE team_id=$1`, `SELECT count(*) FROM paperboat.team_invitations WHERE team_id=$1`, `SELECT count(*) FROM paperboat.team_network_preferences WHERE team_id=$1`} {
		if n := count(q, old); n != 0 {
			t.Errorf("a team deleted 40 days ago kept rows: %s -> %d", q, n)
		}
		if n := count(q, recent); n != 1 {
			t.Errorf("a team deleted 5 days ago lost rows: %s -> %d", q, n)
		}
	}
	if n := count(`SELECT count(*) FROM paperboat.teams WHERE team_id IN ($1,$2)`, old, recent); n != 2 {
		t.Fatalf("the team rows must stay as tombstones: %d", n)
	}
}
