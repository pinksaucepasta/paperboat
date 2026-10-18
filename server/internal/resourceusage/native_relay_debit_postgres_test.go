package resourceusage

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/config"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
)

type recordedDebit struct {
	workspace string
	bytes     int64
}

type fakeDebiter struct{ calls []recordedDebit }

func (f *fakeDebiter) DebitWorkspaceBandwidthTx(_ context.Context, _ *db.Tx, workspace string, bytes int64, _ time.Time) (int64, bool, error) {
	f.calls = append(f.calls, recordedDebit{workspace, bytes})
	return bytes, false, nil
}

func (f *fakeDebiter) total() (sum int64) {
	for _, c := range f.calls {
		sum += c.bytes
	}
	return sum
}

// F13-02: native relay bytes on a hosted node are charged to the workspace allowance, once,
// by the growth of each cumulative per-minute record; direct bytes and customer-run or
// unidentified relays are not.
func TestNativeRelayBytesAreDebitedOnceByGrowthPostgres(t *testing.T) {
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
	defer store.Close()
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
		_, _ = store.SQL().ExecContext(context.Background(), `DELETE FROM users WHERE id=$1`, user)
	})
	exec(`INSERT INTO users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@usage.invalid','active')`, user)
	exec(`INSERT INTO user_machines(id,user_id,environment_id,platform,architecture,workspace_root,state,seat_state,online,installation_generation) VALUES($1,$2,$1,'linux','amd64','/workspace','online','occupied',true,1)`, machine, user)
	exec(`INSERT INTO cli_client_sessions(id,user_id,client_id,client_label,device_type,os,scopes,state,created_at,approved_at) VALUES($1,$2,$1,'Usage','desktop','linux',ARRAY['account:read'],'active',$3,$3)`, client, user, now)
	exec(`INSERT INTO user_machine_access_sessions(credential_kind,id,user_machine_id,user_id,environment_id,cli_client_session_id,http_base_url,helper_terminal_session_id,expires_at,created_at,capabilities) VALUES('control_plane_signed',$1,$2,$3,$2,$4,'','fixture',$5,$6,ARRAY['files'])`, session, machine, user, client, now.Add(time.Hour), now.Add(-time.Minute))
	exec(`INSERT INTO control_tunnel_nodes(id,edge_pool,protocol_version,process_epoch) VALUES($1,'default','1.0',$1)`, node)

	debiter := &fakeDebiter{}
	service := New(store)
	service.Debiter = debiter
	report := func(stream, mode, nodeID string, up, down int64) NativeReport {
		return NativeReport{AccessSessionID: session, StreamID: stream, Consumer: "file_transfer", Mode: mode, NodeID: nodeID, IntervalStart: now, IntervalEnd: now.Add(20 * time.Second), Upload: up, Download: down}
	}
	send := func(r NativeReport) {
		t.Helper()
		receipt, err := service.RecordNative(ctx, machine, 1, NativeBatch{Reports: []NativeReport{r}})
		if err != nil || receipt.Accepted != 1 {
			t.Fatalf("report %+v: %+v %v", r, receipt, err)
		}
	}
	want := func(total int64, why string) {
		t.Helper()
		if got := debiter.total(); got != total {
			t.Fatalf("%s: debited %d bytes, want %d (calls %v)", why, got, total, debiter.calls)
		}
	}

	send(report("direct", "direct", "", 1000, 1000))
	want(0, "direct bytes are free")
	send(report("unnamed", "relay", "", 500, 500))
	want(0, "a relay that names no node cannot be shown to be hosted")
	send(report("hosted", "relay", node, 100, 50))
	want(150, "first hosted relay report")
	send(report("hosted", "relay", node, 100, 50))
	want(150, "a replayed report charges nothing")
	send(report("hosted", "relay", node, 80, 20))
	want(150, "a lower out-of-order report charges nothing")
	send(report("hosted", "relay", node, 300, 50))
	want(350, "only the growth of the cumulative record is charged")
	for _, c := range debiter.calls {
		if c.workspace != "personal:"+user {
			t.Fatalf("charged workspace %q, want the host owner's personal workspace", c.workspace)
		}
	}
}
