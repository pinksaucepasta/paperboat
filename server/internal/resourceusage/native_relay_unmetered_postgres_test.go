package resourceusage

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/billing"
	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

// Native relay bytes on a hosted node are recorded once, as the greatest cumulative counters of each per-minute record, and each
// growth is charged once to the workspace's pool (the same allowance as edge traffic); direct bytes are recorded and never charged.
func TestNativeRelayBytesAreRecordedAndChargedOncePostgres(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("isolated PostgreSQL required")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	store, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() }) // the first cleanup runs last: later cleanups still use store
	ctx := context.Background()
	if err := db.Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Minute).Add(-time.Minute)
	x := fmt.Sprint(time.Now().UnixNano())
	user, machine, session, client, node := "relay_debit_u_"+x, "relay_debit_m_"+x, "relay_debit_s_"+x, "relay_debit_c_"+x, "relay_debit_n_"+x
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		for _, q := range []string{
			`DELETE FROM resource_bandwidth_events WHERE machine_id=$1`,
			`DELETE FROM native_usage_reporting_status WHERE machine_id=$1`,
			`DELETE FROM native_usage_authorities WHERE machine_id=$1`,
			`DELETE FROM user_machine_access_sessions WHERE user_machine_id=$1`,
			`DELETE FROM user_machines WHERE id=$1`,
		} {
			_, _ = store.SQL().ExecContext(context.Background(), q, machine)
		}
		_, _ = store.SQL().ExecContext(context.Background(), `DELETE FROM cli_client_sessions WHERE id=$1`, client)
		_, _ = store.SQL().ExecContext(context.Background(), `DELETE FROM control_tunnel_nodes WHERE id=$1`, node)
		_, _ = store.SQL().ExecContext(context.Background(), `DELETE FROM billing_workspace_usage_periods WHERE workspace_id='personal:'||$1`, user)
		_, _ = store.SQL().ExecContext(context.Background(), `DELETE FROM billing_workspaces WHERE id='personal:'||$1`, user)
		_, _ = store.SQL().ExecContext(context.Background(), `DELETE FROM users WHERE id=$1`, user)
	})
	exec(`INSERT INTO users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@usage.invalid','active')`, user)
	exec(`INSERT INTO user_machines(id,user_id,environment_id,platform,architecture,workspace_root,state,seat_state,online,installation_generation) VALUES($1,$2,$1,'linux','amd64','/workspace','online','occupied',true,1)`, machine, user)
	exec(`INSERT INTO cli_client_sessions(id,user_id,client_id,client_label,device_type,os,scopes,state,created_at,approved_at) VALUES($1,$2,$1,'Usage','desktop','linux',ARRAY['account:read'],'active',$3,$3)`, client, user, now)
	exec(`INSERT INTO user_machine_access_sessions(credential_kind,id,user_machine_id,user_id,environment_id,cli_client_session_id,http_base_url,helper_terminal_session_id,expires_at,created_at,capabilities) VALUES('control_plane_signed',$1,$2,$3,$2,$4,'','fixture',$5,$6,ARRAY['files'])`, session, machine, user, client, now.Add(time.Hour), now.Add(-time.Minute))
	exec(`INSERT INTO control_tunnel_nodes(id,edge_pool,protocol_version,process_epoch) VALUES($1,'default','1.0',$1)`, node)

	service := New(store)
	service.Debiter = billing.NewWorkspaceService(store, nil, "")
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO billing_operator_policy(singleton,personal_allowance_bytes,team_allowance_bytes) VALUES(true,NULL,NULL) ON CONFLICT DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	report := func(stream, mode, nodeID string, up, down int64) NativeReport {
		return NativeReport{AccessSessionID: session, StreamID: stream, Consumer: "file_transfer", Mode: mode, NodeID: nodeID, IntervalStart: now, IntervalEnd: now.Add(20 * time.Second), UploadBytes: up, DownloadBytes: down}
	}
	send := func(r NativeReport) {
		t.Helper()
		receipt, err := service.RecordNative(ctx, machine, 1, NativeBatch{Reports: []NativeReport{r}})
		if err != nil || receipt.Accepted != 1 {
			t.Fatalf("report %+v: %+v %v", r, receipt, err)
		}
	}
	want := func(up, down int64, why string) {
		t.Helper()
		var gotUp, gotDown int64
		if err := store.SQL().QueryRowContext(ctx, `SELECT coalesce(sum(upload_bytes),0),coalesce(sum(download_bytes),0) FROM resource_bandwidth_events WHERE machine_id=$1`, machine).Scan(&gotUp, &gotDown); err != nil {
			t.Fatal(err)
		}
		if gotUp != up || gotDown != down {
			t.Fatalf("%s: recorded %d up %d down, want %d/%d", why, gotUp, gotDown, up, down)
		}
	}

	send(report("hosted", "relay", node, 100, 50))
	want(100, 50, "first hosted relay report")
	send(report("hosted", "relay", node, 100, 50))
	want(100, 50, "a replayed report records nothing more")
	send(report("hosted", "relay", node, 80, 20))
	want(100, 50, "a lower out-of-order report keeps the greater counters")
	send(report("hosted", "relay", node, 300, 50))
	want(300, 50, "the cumulative record grows")
	send(report("direct", "direct", "", 1000, 1000))
	want(1300, 1050, "direct bytes are recorded")
	// The same report posted by several connections at once is recorded once: the batches of one machine run one at a time.
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			receipt, err := service.RecordNative(ctx, machine, 1, NativeBatch{Reports: []NativeReport{report("burst", "relay", node, 200, 100)}})
			if err != nil || receipt.Accepted != 1 {
				t.Errorf("concurrent replay: %+v %v", receipt, err)
			}
		}()
	}
	wg.Wait()
	want(1500, 1150, "a report replayed concurrently is recorded once")
	// Hosted relay growth: 300+50 on the first stream, 200+100 on the burst stream; the replays and the lower report add nothing.
	var observed int64
	if err := store.SQL().QueryRowContext(ctx, `SELECT coalesce(sum(observed_bytes),0) FROM billing_workspace_usage_periods WHERE workspace_id='personal:'||$1`, user).Scan(&observed); err != nil || observed != 650 {
		t.Fatalf("workspace observed %d bytes (err %v), want 650: relay growth only, once, no direct bytes", observed, err)
	}
}
