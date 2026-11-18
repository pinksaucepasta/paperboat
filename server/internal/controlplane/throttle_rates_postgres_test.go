package controlplane

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	wire "github.com/pinksaucepasta/paperboat/shared/connectorprotocol"
)

// A throttled workspace's entry carries this edge's share of its plan's slow speed: Free's or a paid plan's rate, divided
// by the number of edges that carry the workspace (so the workspace as a whole stays at the plan's rate), never below the floor.
func TestThrottleRatesFollowThePlanAndTheEdgesCarryingTheWorkspacePostgres(t *testing.T) {
	store := openControlPlaneTestDB(t)
	ctx := context.Background()
	x := fmt.Sprint(time.Now().UnixNano())
	free, paid := "tr_free_"+x, "tr_paid_"+x
	nodes := []string{"tr_node_a_" + x, "tr_node_b_" + x}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		bg := context.Background()
		for _, q := range []string{
			`DELETE FROM usage_accounting_authorities WHERE environment_id=$1`,
			`DELETE FROM billing_workspaces WHERE id IN ('personal:'||$2,'personal:'||$3)`,
			`DELETE FROM control_tunnel_nodes WHERE id IN ($4,$5)`,
			`DELETE FROM users WHERE id IN ($2,$3)`,
		} {
			_, _ = store.SQL().ExecContext(bg, q, "env_"+x, free, paid, nodes[0], nodes[1])
		}
	})
	for _, u := range []string{free, paid} {
		exec(`INSERT INTO users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@example.test','active')`, u)
	}
	for _, n := range nodes {
		exec(`INSERT INTO control_tunnel_nodes(id,edge_pool,protocol_version,process_epoch) VALUES($1,'default','1.0',$1)`, n)
	}
	exec(`INSERT INTO billing_workspaces(id,user_id) VALUES($1,$2)`, "personal:"+free, free)
	exec(`INSERT INTO billing_workspaces(id,user_id,state,product_code,quantity,period_start,period_end) VALUES($1,$2,'active','pro-month',1,now()-interval '1 hour',now()+interval '1 hour')`, "personal:"+paid, paid)
	carry := func(workspace, node, route string) {
		t.Helper()
		exec(`INSERT INTO usage_accounting_authorities(authority_id,edge_node_id,environment_id,route_id,route_revision,workspace_id,admitted_at,admission_expires_at,retain_until)
 VALUES($1,$2,$3,$4,1,$5,now()-interval '1 hour',now()+interval '1 minute',now()+interval '1 day')`, "tr_a_"+route+"_"+x, node, "env_"+x, route, workspace)
	}
	service := &EdgeService{store: store}
	service.SetFairUse(config.Default().Billing.FairUse)
	rates := func(workspace string) int64 {
		t.Helper()
		entries := []wire.IngressThrottle{{Hostname: "a.example.test", Workspace: workspace}, {Hostname: "b.example.test", Workspace: workspace}}
		if err := service.assignThrottleRates(ctx, entries); err != nil {
			t.Fatal(err)
		}
		if entries[0].BytesPerSecond != entries[1].BytesPerSecond {
			t.Fatalf("routes of one workspace got different rates: %+v", entries)
		}
		return entries[0].BytesPerSecond
	}
	carry("personal:"+free, nodes[0], "r1")
	carry("personal:"+paid, nodes[0], "r2")
	if got := rates("personal:" + free); got != 64_000 {
		t.Fatalf("Free on one edge: %d B/s, want 64000 (512 kbit/s)", got)
	}
	if got := rates("personal:" + paid); got != 125_000 {
		t.Fatalf("a paid plan on one edge: %d B/s, want 125000 (1 Mbit/s)", got)
	}
	carry("personal:"+paid, nodes[1], "r3")
	if got := rates("personal:" + paid); got != 62_500 {
		t.Fatalf("a paid plan on two edges: %d B/s each, want 62500 (the workspace stays at 1 Mbit/s)", got)
	}
	if got := rates("personal:" + free); got != 64_000 {
		t.Fatalf("another workspace's edges changed Free's share: %d", got)
	}
	// An authority that is no longer admitted does not count.
	exec(`UPDATE usage_accounting_authorities SET admission_expires_at=now()-interval '1 second' WHERE route_id='r3' AND environment_id=$1`, "env_"+x)
	if got := rates("personal:" + paid); got != 125_000 {
		t.Fatalf("an expired admission still split the rate: %d", got)
	}
}
