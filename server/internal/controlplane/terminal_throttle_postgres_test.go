package controlplane

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/bandwidthgate"
)

// A console browser terminal's typing and output are not counted toward the allowance and never slowed by it, but the workspace's
// terminal traffic is capped (config.FairUse.TerminalMbit): an edge's snapshot lists the runtime hostname of every machine with an
// attached terminal on it as a Terminal entry carrying this edge's share of the cap (the cap divided by the edges the workspace's
// terminals are attached on). Its FILE transfers meet the workspace's allowance: listed under their own key only while the workspace is
// past its included traffic. Nothing is listed for a terminal on another edge or one that detached.
func TestAttachedTerminalsAreListedWithTheWorkspaceCapForTheEdgePostgres(t *testing.T) {
	store := openControlPlaneTestDB(t)
	ctx := t.Context()
	x := fmt.Sprint(time.Now().UnixNano())
	over, within := "tt_over_"+x, "tt_within_"+x
	node, other := "tt_node_"+x, "tt_other_"+x
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		bg := context.Background()
		for _, u := range []string{over, within} {
			_, _ = store.SQL().ExecContext(bg, `DELETE FROM native_usage_authorities WHERE machine_id='tt_m_'||$1`, u)
			_, _ = store.SQL().ExecContext(bg, `DELETE FROM control_routes WHERE environment_id='tt_env_'||$1`, u)
			_, _ = store.SQL().ExecContext(bg, `DELETE FROM control_environments WHERE id='tt_env_'||$1`, u)
			_, _ = store.SQL().ExecContext(bg, `DELETE FROM user_machines WHERE id='tt_m_'||$1`, u)
			_, _ = store.SQL().ExecContext(bg, `DELETE FROM billing_workspace_usage_periods WHERE workspace_id='personal:'||$1`, u)
			_, _ = store.SQL().ExecContext(bg, `DELETE FROM billing_workspaces WHERE id='personal:'||$1`, u)
			_, _ = store.SQL().ExecContext(bg, `DELETE FROM users WHERE id=$1`, u)
		}
		_, _ = store.SQL().ExecContext(bg, `DELETE FROM control_tunnel_nodes WHERE id IN ($1,$2)`, node, other)
	})
	for _, n := range []string{node, other} {
		exec(`INSERT INTO control_tunnel_nodes(id,edge_pool,protocol_version,process_epoch) VALUES($1,'default','1.0',$1)`, n)
	}
	now := time.Now().UTC()
	attach := func(user string, consumed int64, edge string) string {
		exec(`INSERT INTO users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@example.test','active')`, user)
		machine, env, host := "tt_m_"+user, "tt_env_"+user, "tt-"+user+".runtime.example.test"
		exec(`INSERT INTO user_machines(id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state,online) VALUES($1,$2,$3,'tt','linux','amd64','/w','online','occupied',true)`, machine, user, env)
		exec(`INSERT INTO control_environments(id,workspace_id) VALUES($1,$2)`, env, "ws_"+user)
		exec(`INSERT INTO control_routes(id,environment_id,kind,public_host,target_host,target_port,applied_node_id) VALUES($1,$2,'runtime_https_wss',$3,'127.0.0.1',8443,$4)`, "tt_r_"+user, env, host, node)
		exec(`INSERT INTO billing_workspaces(id,user_id,state) VALUES('personal:'||$1,$1,'free')`, user)
		exec(`INSERT INTO billing_workspace_usage_periods(workspace_id,period_start,period_end,included_bytes,consumed_bytes,paid,budget_start) VALUES('personal:'||$1,$2,$3,100,$4,false,$2)`, user, now.Add(-time.Hour), now.AddDate(0, 1, 0), consumed)
		exec(`INSERT INTO native_usage_authorities(access_session_id,machine_id,installation_generation,workspace_id,resource_kind,resource_id,resource_name,machine_name,capabilities,admitted_at,expires_at,retain_until,edge_node_id,attached_until)
 VALUES($1,$2,1,'personal:'||$3,'terminal','term','t','tt',ARRAY['terminal'],now(),now()+interval '5 minutes',now()+interval '7 days',$4,now()+interval '1 minute')`, "att_"+user, machine, user, edge)
		return host
	}
	attach(over, 100, node)
	attach(within, 5, node)

	service := NewEdgeService(store, "terminal-throttle-test-credential-0123456789")
	gate := bandwidthgate.New(store)
	gate.SetLookupTimeout(5 * time.Second) // this test database is slower than production
	service.workspaceGate = gate
	capBytes := int64(service.fairUse.TerminalMbit * 125_000)
	attached := func(edge string) []attachedTerminal {
		t.Helper()
		got, err := service.attachedTerminals(ctx, edge)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	got := service.addTerminalCaps(attached(node), nil)
	if len(got) != 2 {
		t.Fatalf("terminal caps = %+v, want both attached terminals (over its allowance or not)", got)
	}
	for _, c := range got {
		if !c.Terminal || c.BytesPerSecond != capBytes {
			t.Fatalf("entry %+v, want a terminal entry at the whole cap %d (one edge)", c, capBytes)
		}
	}
	// File transfers through a terminal meet the workspace's ALLOWANCE: listed (under their own key) only for the workspace past it.
	files := service.addFileThrottles(ctx, attached(node), nil)
	if len(files) != 1 || files[0].Terminal || files[0].Workspace != "personal:"+over || files[0].Hostname != fileThrottleHost(got[0].Hostname) && files[0].Hostname != fileThrottleHost(got[1].Hostname) {
		t.Fatalf("file throttles = %+v, want only the workspace past its included traffic", files)
	}
	if elsewhere := attached(other); len(elsewhere) != 0 {
		t.Fatalf("another edge listed %+v", elsewhere)
	}
	// The same workspace attached on a second edge: each edge gets half of the cap.
	exec(`INSERT INTO native_usage_authorities(access_session_id,machine_id,installation_generation,workspace_id,resource_kind,resource_id,resource_name,machine_name,capabilities,admitted_at,expires_at,retain_until,edge_node_id,attached_until)
 VALUES($1,$2,1,'personal:'||$3,'terminal','term2','t','tt',ARRAY['terminal'],now(),now()+interval '5 minutes',now()+interval '7 days',$4,now()+interval '1 minute')`, "att2_"+over, "tt_m_"+over, over, other)
	for _, c := range service.addTerminalCaps(attached(node), nil) {
		if c.Workspace == "personal:"+over && c.BytesPerSecond != capBytes/2 {
			t.Fatalf("workspace on two edges: %+v, want half the cap %d", c, capBytes/2)
		}
	}
	exec(`UPDATE native_usage_authorities SET attached_until=now()-interval '1 second' WHERE machine_id=$1`, "tt_m_"+over)
	exec(`UPDATE native_usage_authorities SET expires_at=now()-interval '1 second',admitted_at=now()-interval '1 hour' WHERE machine_id=$1`, "tt_m_"+over)
	detached := attached(node)
	if len(detached) != 1 || detached[0].workspace != "personal:"+within {
		t.Fatalf("a detached terminal is still listed: %+v", detached)
	}
}
