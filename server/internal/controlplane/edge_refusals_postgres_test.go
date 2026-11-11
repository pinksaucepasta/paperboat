package controlplane

import (
	"context"
	"fmt"
	"testing"
	"time"

	wire "github.com/pinksaucepasta/paperboat/shared/connectorprotocol"
)

// A heartbeat's flood-protection refusals add up per hostname, edge and hour; malformed entries are
// skipped, and counts past retention are deleted by the same writer.
func TestEdgeRefusalsAccumulateHourlyAndExpire(t *testing.T) {
	store := openControlPlaneTestDB(t)
	ctx := context.Background()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	host, old := "busy-"+suffix+".example.test", "old-"+suffix+".example.test"
	node := "node_refusals_" + suffix
	t.Cleanup(func() {
		_, _ = store.SQL().ExecContext(ctx, `DELETE FROM paperboat.edge_refusals WHERE edge_node_id=$1`, node)
	})
	now := time.Date(2026, 11, 11, 14, 20, 0, 0, time.UTC)
	service := NewEdgeService(store, "credential")
	service.clock = func() time.Time { return now }
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.edge_refusals(hostname,edge_node_id,hour,refused) VALUES($1,$2,$3,5)`, old, node, now.Add(-edgeRefusalRetention-time.Hour)); err != nil {
		t.Fatal(err)
	}
	service.recordEdgeRefusals(ctx, node, []edgeHostRefusals{{Hostname: host, Count: 3}, {Hostname: "", Count: 9}, {Hostname: "neg-" + suffix, Count: -1}})
	service.recordEdgeRefusals(ctx, node, []edgeHostRefusals{{Hostname: host, Count: 4}})
	var refused, rows int64
	var hour time.Time
	if err := store.SQL().QueryRowContext(ctx, `SELECT refused, hour FROM paperboat.edge_refusals WHERE hostname=$1 AND edge_node_id=$2`, host, node).Scan(&refused, &hour); err != nil {
		t.Fatal(err)
	}
	if refused != 7 || !hour.Equal(now.Truncate(time.Hour)) {
		t.Fatalf("refused=%d hour=%v, want 7 in %v", refused, hour, now.Truncate(time.Hour))
	}
	if err := store.SQL().QueryRowContext(ctx, `SELECT count(*) FROM paperboat.edge_refusals WHERE edge_node_id=$1`, node).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("rows=%d err=%v, want only the current count (malformed skipped, expired deleted)", rows, err)
	}
}

// A taken-down hostname's decisions and quota denials are withheld from the edge snapshot and the
// name is listed with its legal flag; other names are untouched.
func TestWithholdBlockedRemovesTakenDownNames(t *testing.T) {
	store := openControlPlaneTestDB(t)
	ctx := context.Background()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	taken, quota, open, restored := "taken-"+suffix+".example.test", "quota-"+suffix+".example.test", "open-"+suffix+".example.test", "restored-"+suffix+".example.test"
	t.Cleanup(func() {
		_, _ = store.SQL().ExecContext(ctx, `DELETE FROM paperboat.publication_blocks WHERE hostname = ANY($1)`, []string{taken, quota, restored})
	})
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.publication_blocks(hostname,blocked,legal,reason) VALUES($1,true,true,'case'),($2,true,false,'case'),($3,false,false,'case')`, taken, quota, restored); err != nil {
		t.Fatal(err)
	}
	decision := func(host string) wire.IngressDecision {
		var d wire.IngressDecision
		d.Binding.Hostname = host
		return d
	}
	decisions := []wire.IngressDecision{decision(taken), decision(open), decision(restored)}
	denied := []wire.IngressQuotaDenial{{Hostname: quota}}
	blocked, err := withholdBlocked(ctx, store, &decisions, &denied)
	if err != nil {
		t.Fatal(err)
	}
	if len(decisions) != 2 || decisions[0].Binding.Hostname != open || decisions[1].Binding.Hostname != restored || len(denied) != 0 {
		t.Fatalf("decisions=%v denied=%v", decisions, denied)
	}
	got := map[string]bool{}
	for _, b := range blocked {
		got[b.Hostname] = b.Legal
	}
	if len(got) != 2 || got[taken] != true || got[quota] != false {
		t.Fatalf("blocked=%v", blocked)
	}
}

// A tunnel's rules reach the edge through the routes of the tunnel, not through the decision's TunnelID (that is the
// runtime carrier's identity): both routes of the ruled tunnel get its lists, the other tunnel's route none; a preview's
// rules key on its publication.
func TestOwnerIPRulesFollowTheirResource(t *testing.T) {
	store := openControlPlaneTestDB(t)
	ctx := context.Background()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	owner, machine := "user_rules_"+suffix, "host_rules_"+suffix
	ruled, other, preview := "tunnel_rules_"+suffix, "tunnel_other_"+suffix, "prv_rules_"+suffix
	routeA, routeB, routeX := "route_a_"+suffix, "route_b_"+suffix, "route_x_"+suffix
	now := time.Now().UTC().Truncate(time.Microsecond)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = store.SQL().ExecContext(cleanup, `DELETE FROM paperboat.access_ip_rules WHERE resource_id = ANY($1)`, []string{ruled, other, preview})
		_, _ = store.SQL().ExecContext(cleanup, `DELETE FROM paperboat.tunnels WHERE id = ANY($1)`, []string{ruled, other})
		_, _ = store.SQL().ExecContext(cleanup, `DELETE FROM paperboat.user_machines WHERE id=$1`, machine)
		_, _ = store.SQL().ExecContext(cleanup, `DELETE FROM paperboat.users WHERE id=$1`, owner)
	})
	exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,email_verified,status) VALUES($1,$1,'owner@test.invalid',false,'active')`, owner)
	exec(`INSERT INTO paperboat.user_machines(id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state) VALUES($1,$2,$3,'rules-host','linux','amd64','/workspace','offline','released')`, machine, owner, "env_rules_"+suffix)
	for i, tunnel := range []string{ruled, other} {
		endpoint := fmt.Sprintf("%08d-0000-4000-8000-%s", i, suffix[len(suffix)-12:])
		exec(`INSERT INTO paperboat.tunnels(id,account_id,name,desired_state,access_mode,generation,stable_endpoint_id,stable_endpoint,created_by_host_id,created_by_actor_id,created_at,updated_at) VALUES($1,$2,$3,'active','public',1,$4,$5,$6,$2,$7,$7)`, tunnel, owner, "rules-"+tunnel, "endpoint_"+endpoint, "https://"+endpoint+".tunnels.example.test", machine, now)
	}
	for _, route := range []struct{ id, tunnel string }{{routeA, ruled}, {routeB, ruled}, {routeX, other}} {
		exec(`INSERT INTO paperboat.tunnel_routes(id,tunnel_id,name,protocol,match_type,match_hostname,path_prefix,priority,origin_scheme,origin_address,desired_state,generation,created_by_actor_id,updated_by_actor_id,created_at,updated_at) VALUES($1,$2,$1,'http','managed',$1,'/',100,'http','127.0.0.1:3100','active',1,$3,$3,$4,$4)`, route.id, route.tunnel, owner, now)
	}
	exec(`INSERT INTO paperboat.access_ip_rules(resource_kind,resource_id,allow_cidrs,deny_cidrs,updated_by) VALUES('tunnel',$1,'{198.51.100.0/24}','{}','test'),('preview',$2,'{}','{203.0.113.0/24}','test')`, ruled, preview)
	decision := func(host, carrierTunnel, route, publication, lifecycle string) wire.IngressDecision {
		var d wire.IngressDecision
		d.Binding.Hostname, d.Binding.TunnelID, d.Binding.RouteID, d.Binding.PublicationID, d.Binding.Lifecycle = host, carrierTunnel, route, publication, lifecycle
		return d
	}
	rules, err := ownerIPRules(ctx, store, []wire.IngressDecision{
		decision("a.example.test", "carrier_a", routeA, routeA, wire.TunnelDurable),
		decision("b.example.test", "carrier_b", routeB, routeB, wire.TunnelDurable),
		decision("p.example.test", "carrier_p", "", preview, wire.TunnelEphemeral),
		decision("other.example.test", "carrier_x", routeX, routeX, wire.TunnelDurable),
	})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]wire.IngressIPRule{}
	for _, r := range rules {
		got[r.Hostname] = r
	}
	if len(got) != 3 || len(got["a.example.test"].Allow) != 1 || len(got["b.example.test"].Allow) != 1 || len(got["p.example.test"].Deny) != 1 || got["p.example.test"].Deny[0] != "203.0.113.0/24" {
		t.Fatalf("rules = %+v", rules)
	}
}
