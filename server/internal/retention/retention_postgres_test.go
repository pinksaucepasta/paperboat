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
// window in bounded batches, keeps what is inside it, and caps per-account and per-environment history; every
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
	user, env := "ret_user_"+s, "ret_env_"+s
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
	exec(`INSERT INTO paperboat.control_environments(id,workspace_id,owner_user_id,desired_state,desired_version,applied_state,applied_version,created_at,updated_at)
VALUES($1,$1,$2,'active',1,'ready',1,now(),now())`, env, user)
	// 5,000 idempotency records two days old (past the 24 h window) and 10 fresh ones.
	exec(`INSERT INTO paperboat.managed_ssh_operations(operation_id,user_id,operation_kind,request_hash,resource_id,result_revision,created_at)
SELECT 'retention-op-'||$2||'-'||g,$1,'target_update',sha256(convert_to(g::text,'UTF8')),'r'||g,1,CASE WHEN g<=5000 THEN now()-interval '2 days' ELSE now() END
FROM generate_series(1,5010) g`, user, s)
	// 1,050 support reports for one account (cap 1,000) and 3 expired ones.
	exec(`INSERT INTO paperboat.support_reports(id,support_reference,owner_id,request_id,component,release,stage,code,created_at,expires_at,sentry_event_id,
source_file,source_function,source_line,reporting)
SELECT 'ret_report_'||$2||'_'||g,'support_'||gen_random_uuid(),$1,'request_'||g,'paperboat-server','dev','http_handler','internal_error',
 now()-make_interval(secs=>g),CASE WHEN g>1050 THEN now()-interval '1 minute' ELSE now()+interval '7 days' END,'','f.go','f',1,'disabled'
FROM generate_series(1,1053) g`, user, s)
	// 130 sync results for one environment (cap 100).
	exec(`INSERT INTO paperboat.control_config_sync_status_history(environment_id,sync_revision,repository_id,assignment_id,machine_id,installation_generation,state)
SELECT $1,g,'repo','assignment','machine',1,'applied' FROM generate_series(1,130) g`, env)

	cfg := config.Default().Retention
	result, err := Run(ctx, store, cfg)
	if err != nil {
		t.Fatal(err) // every policy's statement ran
	}
	if n := count(`SELECT count(*) FROM paperboat.managed_ssh_operations WHERE user_id=$1`, user); n != 10 || result["managed_ssh_operations"] < 5000 {
		t.Fatalf("managed_ssh_operations: %d left (want the 10 fresh), %d deleted", n, result["managed_ssh_operations"])
	}
	if n := count(`SELECT count(*) FROM paperboat.support_reports WHERE owner_id=$1`, user); n != cfg.SupportReportsPerAccount {
		t.Fatalf("support_reports: %d left for the account, want its newest %d", n, cfg.SupportReportsPerAccount)
	}
	if n := count(`SELECT count(*) FROM paperboat.support_reports WHERE owner_id=$1 AND expires_at<now()`, user); n != 0 {
		t.Fatalf("%d expired support reports kept", n)
	}
	if newest := count(`SELECT min(source_line+0*0) FROM paperboat.support_reports WHERE owner_id=$1 AND id='ret_report_'||$2||'_1'`, user, s); newest != 1 {
		t.Fatal("the account's newest report was deleted")
	}
	if n := count(`SELECT count(*) FROM paperboat.control_config_sync_status_history WHERE environment_id=$1`, env); n != cfg.ConfigSyncHistoryPerEnvironment {
		t.Fatalf("config sync history: %d left, want the newest %d", n, cfg.ConfigSyncHistoryPerEnvironment)
	}
	if oldest := count(`SELECT min(sync_revision) FROM paperboat.control_config_sync_status_history WHERE environment_id=$1`, env); oldest != 31 {
		t.Fatalf("config sync history kept revision %d as its oldest, want 31", oldest)
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
