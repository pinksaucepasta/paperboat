package usermachines

import (
	"context"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/helperruntime"
	"github.com/pinksaucepasta/paperboat/server/internal/workspace"
)

// seedTerminalNoise gives the account `machines` extra machines with 2 sessions each, the fixture machine `sessions` sessions,
// and `others` other accounts with 3 machines x 3 sessions: the shape of a production database for the list/descriptor cost tests.
func seedTerminalNoise(t *testing.T, f workspaceMachineFixture, machines, sessions, others int) {
	t.Helper()
	pre := "tl" + f.suffix
	ctx := context.Background()
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		for _, q := range []string{
			`DELETE FROM paperboat.user_machine_terminal_sessions WHERE id LIKE $1`,
			`DELETE FROM paperboat.user_machines WHERE id LIKE $1`,
			`DELETE FROM paperboat.users WHERE id LIKE $1`,
		} {
			if _, err := f.store.SQL().ExecContext(c, q, pre+"%"); err != nil {
				t.Errorf("cleanup: %v", err)
			}
		}
	})
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) SELECT $1||'u'||g,$1||'u'||g,$1||'u'||g||'@invalid.test','active' FROM generate_series(1,$2::int) g`, []any{pre, others}},
		{`INSERT INTO paperboat.user_machines(id,user_id,environment_id,platform,architecture,workspace_root,state,seat_state,online,configured_capabilities,observed_capabilities)
SELECT $1||'m'||g||'_'||o,$1||'u'||g,$1||'e'||g||'_'||o,'linux','amd64','/workspace','online','occupied',true,ARRAY['terminal_host'],ARRAY['terminal_host'] FROM generate_series(1,$2::int) g, generate_series(1,3) o`, []any{pre, others}},
		{`INSERT INTO paperboat.user_machines(id,user_id,environment_id,platform,architecture,workspace_root,state,seat_state,online,configured_capabilities,observed_capabilities)
SELECT $1||'x'||g,$2,$1||'ex'||g,'linux','amd64','/workspace','online','occupied',true,ARRAY['terminal_host'],ARRAY['terminal_host'] FROM generate_series(1,$3::int) g`, []any{pre, f.account, machines}},
		{`INSERT INTO paperboat.user_machine_terminal_sessions(id,user_machine_id,owner_account,name,idempotency_key,launch_cwd,runtime_state)
SELECT $1||'s'||g||'_'||o||'_'||s,$1||'m'||g||'_'||o,$1||'u'||g,'s'||s,$1||'s'||g||'_'||o||'_'||s,'/workspace','running' FROM generate_series(1,$2::int) g, generate_series(1,3) o, generate_series(1,3) s`, []any{pre, others}},
		{`INSERT INTO paperboat.user_machine_terminal_sessions(id,user_machine_id,owner_account,name,idempotency_key,launch_cwd,runtime_state)
SELECT $1||'sx'||g||'_'||s,$1||'x'||g,$2,'s'||s,$1||'sx'||g||'_'||s,'/workspace','running' FROM generate_series(1,$3::int) g, generate_series(1,2) s`, []any{pre, f.account, machines}},
		{`INSERT INTO paperboat.user_machine_terminal_sessions(id,user_machine_id,owner_account,name,idempotency_key,launch_cwd,runtime_state)
SELECT $1||'sf'||g,$3,$2,'f'||g,$1||'sf'||g,'/workspace','running' FROM generate_series(1,$4::int) g`, []any{pre, f.account, f.machine, sessions}},
		{`ANALYZE paperboat.user_machine_terminal_sessions`, nil},
		{`ANALYZE paperboat.user_machines`, nil},
	} {
		if _, err := f.store.SQL().ExecContext(ctx, q.sql, q.args...); err != nil {
			t.Fatal(err)
		}
	}
}

// delayedRuntime answers a host snapshot after a fixed latency, like a real host behind an edge.
type delayedRuntime struct {
	terminalIdentificationRuntime
	delay time.Duration
}

func (r *delayedRuntime) Terminal(ctx context.Context, route, credential, operation, session, operationID string) (helperruntime.Snapshot, error) {
	select {
	case <-time.After(r.delay):
	case <-ctx.Done():
		return helperruntime.Snapshot{}, ctx.Err()
	}
	return r.terminalIdentificationRuntime.Terminal(ctx, route, credential, operation, session, operationID)
}

// A live page costs one host round trip and one write per listed session (the cost the stored list avoids), so callers that only
// match names or states ask for live=false.
func TestTerminalListLiveCostPostgres(t *testing.T) {
	f, _, signer := newWorkspaceDescriptorFixture(t)
	seedTerminalNoise(t, f, 200, 120, 300)
	if _, err := f.store.SQL().ExecContext(f.ctx, `UPDATE paperboat.control_tunnel_nodes SET public_ingress_verified_at=now(),endpoint_host='edge.example.test' WHERE id=$1`, "ws_edge_"+f.suffix); err != nil {
		t.Fatal(err)
	}
	runtime := &delayedRuntime{terminalIdentificationRuntime: terminalIdentificationRuntime{signer: signer, machine: f.machine, environment: "env_" + f.suffix, account: f.account, route: "https://workspace.invalid.test", multiple: true}, delay: 80 * time.Millisecond}
	f.service.issuer = "https://api.paperboat.test"
	f.service.controlRuntime = runtime
	ctx := workspace.WithContext(f.ctx, workspace.Scope{ID: "personal"})
	for _, live := range []bool{true, false} {
		before := runtime.calls.Load()
		start := time.Now()
		items, total, err := f.service.ListTerminalSessionInventory(ctx, f.account, f.machine, InventoryOptions{Limit: 50, WithoutLiveMetadata: !live})
		elapsed, calls := time.Since(start), runtime.calls.Load()-before
		t.Logf("live=%t: %d of %d sessions in %v, host calls=%d", live, len(items), total, elapsed, calls)
		if err != nil || total < 120 || len(items) != 50 {
			t.Fatalf("list: %d of %d err=%v", len(items), total, err)
		}
		// 200 + 900 other machines and 120 sessions on this one: stored rows are database work only.
		if !live && (elapsed > 250*time.Millisecond || calls != 0) {
			t.Fatalf("a list without live metadata took %v and made %d host calls", elapsed, calls)
		}
		if live && calls != 50 {
			t.Fatalf("a live page made %d host calls, want one per listed session (50)", calls)
		}
	}
}

func TestTerminalDescriptorCostPostgres(t *testing.T) {
	f, source, _ := newWorkspaceDescriptorFixture(t)
	seedTerminalNoise(t, f, 200, 120, 300)
	ctx := workspace.WithContext(f.ctx, workspace.Scope{ID: "personal"})
	session, err := f.service.CreateTerminalSession(ctx, f.account, f.machine, "descriptor", "descriptor_cost"+f.suffix)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.SQL().ExecContext(f.ctx, `DELETE FROM paperboat.user_machine_terminal_session_operations WHERE terminal_session_id=$1`, session.ID); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		start := time.Now()
		d, err := f.service.ConnectTerminalSession(ctx, f.account, source, f.machine, "cli_"+f.suffix, session.ID)
		elapsed := time.Since(start)
		t.Logf("descriptor %d: %v", i, elapsed)
		if err != nil || !d.Connectable || elapsed > 250*time.Millisecond {
			t.Fatalf("descriptor %d: connectable=%t in %v err=%v", i, d.Connectable, elapsed, err)
		}
	}
}
