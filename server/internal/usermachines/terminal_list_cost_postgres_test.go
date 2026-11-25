package usermachines

import (
	"context"
	"reflect"
	"strings"
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

func (r *delayedRuntime) TerminalSnapshots(ctx context.Context, route, credential string, sessions []string, operationID string) (map[string]helperruntime.SnapshotResult, error) {
	select {
	case <-time.After(r.delay):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return r.terminalIdentificationRuntime.TerminalSnapshots(ctx, route, credential, sessions, operationID)
}

// CreateTerminal starts a shell after the same latency (a real create took p50 2.6 s on production).
func (r *delayedRuntime) CreateTerminal(ctx context.Context, route, credential, session, operationID, name, cwd string) (helperruntime.Snapshot, error) {
	r.calls.Add(1)
	select {
	case <-time.After(r.delay):
	case <-ctx.Done():
		return helperruntime.Snapshot{}, ctx.Err()
	}
	return helperruntime.Snapshot{ID: session, Name: name, CWD: cwd, State: "running", Generation: 1}, nil
}

// A live page costs one host round trip per 32 sessions of a machine and one write per listed session (the cost the stored list avoids), so callers that only
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
		if live && calls != 2 {
			t.Fatalf("a live page made %d host calls, want one per 32 sessions (2)", calls)
		}
	}
}

// A page answered in batches lists exactly what one request per session listed, for rows of every kind.
func TestTerminalBatchedPageEqualsPerSessionPostgres(t *testing.T) {
	f, _, signer := newWorkspaceDescriptorFixture(t)
	seedTerminalNoise(t, f, 2, 100, 2)
	if _, err := f.store.SQL().ExecContext(f.ctx, `UPDATE paperboat.control_tunnel_nodes SET public_ingress_verified_at=now(),endpoint_host='edge.example.test' WHERE id=$1`, "ws_edge_"+f.suffix); err != nil {
		t.Fatal(err)
	}
	pre := "tl" + f.suffix
	runtime := &delayedRuntime{terminalIdentificationRuntime: terminalIdentificationRuntime{signer: signer, machine: f.machine, environment: "env_" + f.suffix, account: f.account, route: "https://workspace.invalid.test", multiple: true}}
	f.service.issuer, f.service.controlRuntime = "https://api.paperboat.test", runtime
	ctx := workspace.WithContext(f.ctx, workspace.Scope{ID: "personal"})
	closed := false
	items, _, err := f.service.TerminalSessionInventory(ctx, f.account, InventoryOptions{Limit: 200, Closed: &closed, WithoutLiveMetadata: true})
	if err != nil || len(items) < 100 {
		t.Fatalf("rows: %d err=%v", len(items), err)
	}
	// Besides live rows of the online machine and rows of machines without a route: a shared one, an offline one, one
	// in a team the owner is no longer in (no credential), and one the host no longer has.
	live := []int{}
	for index := range items {
		if items[index].Target.MachineID == f.machine {
			live = append(live, index)
		}
	}
	if len(live) < 100 {
		t.Fatalf("live rows: %d", len(live))
	}
	items[live[1]].Sharing.TeamID, items[live[1]].CanManage = "team_shared", false
	items[live[2]].Target.Status = "offline"
	revoked, gone := items[live[3]].ID, items[live[4]].ID
	runtime.absent = map[string]bool{gone: true}
	if _, err := f.store.SQL().ExecContext(f.ctx, `UPDATE paperboat.user_machine_terminal_sessions SET runtime_state='running' WHERE id LIKE $1`, pre+"%"); err != nil {
		t.Fatal(err)
	}
	page, err := f.service.loadTerminalPage(ctx, items)
	if err != nil {
		t.Fatal(err)
	}
	page.launch[revoked] = "team_left"
	grant := SharedTerminalGrant{Audience: "all_members", Role: "viewer", Active: true}
	items[live[5]].CanManage = true
	for _, index := range []int{live[1], live[5]} {
		page.grants[items[index].ID] = []SharedTerminalGrant{grant}
	}
	for _, host := range []string{"current", "unavailable", "older"} {
		runtime.unavailable, runtime.older = host == "unavailable", host == "older"
		started, before := time.Now(), runtime.calls.Load()
		batched := make([]*SharedTerminalSession, len(items))
		for _, group := range page.groups(items) {
			for index, item := range f.service.describeTerminalGroup(ctx, f.account, items, group, page) {
				batched[index] = item
			}
		}
		t.Logf("%s host: batched %v, %d host calls", host, time.Since(started), runtime.calls.Load()-before)
		if calls := runtime.calls.Load() - before; host == "current" && calls > 4 {
			t.Fatalf("%d host calls for %d live rows", calls, len(live))
		}
		for index, item := range items {
			single, err := f.service.describeTerminalSession(ctx, f.account, item, page)
			if err != nil {
				t.Fatal(err)
			}
			got := batched[index]
			if got == nil {
				got = &item // a row without an answer is listed as stored
			}
			if (index == live[1] || index == live[5]) && (len(got.Sharing.Grants) != 1 || got.Sharing.Audience != "all_members") {
				t.Fatalf("%s host: row lost its team grants: %+v", host, got.Sharing)
			}
			if host == "older" && index == live[6] && !got.ParticipantsAvailable {
				t.Fatalf("a host without batched snapshots lost live detail: %+v", got)
			}
			single.ExpiresAt, got.ExpiresAt = nil, nil // two reads of now()
			if !reflect.DeepEqual(*got, single) {
				t.Fatalf("row %s\nbatched %+v\nsingle  %+v", item.ID, *got, single)
			}
		}
	}
	runtime.unavailable, runtime.older, runtime.delay = false, false, 150*time.Millisecond
	started := time.Now()
	listed, _, err := f.service.TerminalSessionInventory(ctx, f.account, InventoryOptions{Limit: 200, Closed: &closed})
	t.Logf("live list of %d: %v (150 ms to the host)", len(listed), time.Since(started))
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range listed {
		if item.Target.MachineID == f.machine && item.ID != gone && strings.HasPrefix(item.ID, pre) && !item.ParticipantsAvailable {
			t.Fatalf("row %s lost its live detail", item.ID)
		}
	}
}

func TestTerminalDescriptorCostPostgres(t *testing.T) {
	f, source, _ := newWorkspaceDescriptorFixture(t)
	seedTerminalNoise(t, f, 200, 120, 300)
	ctx := workspace.WithContext(f.ctx, workspace.Scope{ID: "personal"})
	// The new session's create is still pending and the host takes 2 s to start a shell: the descriptor leaves the
	// create to the CLI's create-or-get on attach instead of waiting for it.
	runtime := &delayedRuntime{delay: 2 * time.Second}
	f.service.controlRuntime = runtime
	session, err := f.service.CreateTerminalSession(ctx, f.account, f.machine, "descriptor", "descriptor_cost"+f.suffix)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		var pending bool
		if err := f.store.SQL().QueryRowContext(f.ctx, `SELECT EXISTS(SELECT 1 FROM paperboat.user_machine_terminal_session_operations WHERE terminal_session_id=$1 AND operation='create' AND state='pending')`, session.ID).Scan(&pending); err != nil || !pending || runtime.calls.Load() != 0 {
			t.Fatalf("create pending for the worker = %t, host calls = %d, err = %v", pending, runtime.calls.Load(), err)
		}
	}()
	for i := 0; i < 3; i++ {
		start := time.Now()
		d, err := f.service.ConnectTerminalSession(ctx, f.account, source, f.machine, "cli_"+f.suffix, session.ID)
		elapsed := time.Since(start)
		t.Logf("descriptor %d: %v", i, elapsed)
		// Waiting for the host's shell start would take runtime.delay (2 s); the database work around it is ~25 ms
		// and grows with the noise rows earlier iterations leave behind (435 ms under -race on 2 cores), so the bound is
		// half the host's delay: the property is "does not wait for the host", not a latency budget.
		if err != nil || !d.Connectable || elapsed > runtime.delay/2 {
			t.Fatalf("descriptor %d: connectable=%t in %v err=%v", i, d.Connectable, elapsed, err)
		}
	}
}
