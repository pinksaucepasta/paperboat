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

// The hostnames of a banned account's and a banned team's tunnels and previews are listed for the edge, which answers the
// takedown page for them even when no decision is left; a lifted ban, another owner and a removed endpoint list nothing.
func TestWithholdBlockedListsBannedOwnersHostnames(t *testing.T) {
	store := openControlPlaneTestDB(t)
	ctx := context.Background()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	owner, other, team, machine, otherMachine := "user_ban_"+suffix, "user_free_"+suffix, "team-ban-"+suffix, "host_ban_"+suffix, "host_free_"+suffix
	hosts := map[string]string{"banned": "banned-" + suffix + ".example.test", "team": "teamed-" + suffix + ".example.test", "free": "free-" + suffix + ".example.test", "lifted": "lifted-" + suffix + ".example.test", "removed": "removed-" + suffix + ".example.test"}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		bg := context.Background()
		store.SQL().ExecContext(bg, `DELETE FROM paperboat.bans WHERE subject_id = ANY($1)`, []string{owner, other, team})
		store.SQL().ExecContext(bg, `DELETE FROM paperboat.endpoints WHERE account_id = ANY($1)`, []string{owner, other})
		store.SQL().ExecContext(bg, `DELETE FROM paperboat.tunnels WHERE account_id = ANY($1)`, []string{owner, other})
		store.SQL().ExecContext(bg, `DELETE FROM paperboat.user_machines WHERE id = ANY($1)`, []string{machine, otherMachine})
		store.SQL().ExecContext(bg, `DELETE FROM paperboat.teams WHERE team_id=$1`, team)
		store.SQL().ExecContext(bg, `DELETE FROM paperboat.users WHERE id = ANY($1)`, []string{owner, other})
	})
	for _, u := range []string{owner, other} {
		exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,email_verified,status) VALUES($1,$1,$1||'@test.invalid',false,'active')`, u)
	}
	exec(`INSERT INTO paperboat.teams(team_id,owner_account,generation) VALUES($1,$2,1)`, team, other)
	for _, m := range [][2]string{{machine, owner}, {otherMachine, other}} {
		exec(`INSERT INTO paperboat.user_machines(id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state) VALUES($1,$2,$3,'a'||substr(md5($1),1,12),'linux','amd64','/workspace','offline','released')`, m[0], m[1], "env_"+m[0])
	}
	n := 0
	endpoint := func(name, account, agent, team, state string, host string) {
		t.Helper()
		var teamID any
		if team != "" {
			teamID = team
		}
		n++
		stable := fmt.Sprintf("%08d-0000-4000-8000-%s", n, suffix[len(suffix)-12:])
		exec(`INSERT INTO paperboat.tunnels(id,account_id,name,desired_state,access_mode,generation,stable_endpoint_id,stable_endpoint,created_by_host_id,created_by_actor_id,created_at,updated_at) VALUES($1,$2,$3,'active','public',1,$4,$5,$6,$2,now(),now())`, "tn_"+name+suffix, account, "ban-"+name+suffix, "endpoint_"+stable, "https://"+stable+".tunnels.example.test", agent)
		exec(`INSERT INTO paperboat.endpoints(id,kind,account_id,owner_team_id,agent_machine_id,access_mode,lifetime,desired_state,tunnel_id) VALUES($1,'tunnel',$2,$3,$4,'public','persistent',$5,$6)`, "ep_"+name+suffix, account, teamID, agent, state, "tn_"+name+suffix)
		exec(`INSERT INTO paperboat.endpoint_routes(id,endpoint_id,hostname,protocol) VALUES($1,$2,$3,'http')`, "route_"+name+suffix, "ep_"+name+suffix, host)
	}
	endpoint("banned", owner, machine, "", "active", hosts["banned"])
	endpoint("removed", owner, machine, "", "removed", hosts["removed"])
	endpoint("team", other, otherMachine, team, "active", hosts["team"])
	endpoint("free", other, otherMachine, "", "active", hosts["free"])
	endpoint("lifted", other, otherMachine, "", "active", hosts["lifted"])
	// A tunnel's own verified custom domain is a hostname of its owner too.
	custom := map[string]string{"banned": "blog-" + suffix + ".example.test", "free": "shop-" + suffix + ".example.test"}
	for name, host := range custom {
		exec(`INSERT INTO paperboat.tunnel_routes(id,tunnel_id,name,protocol,match_type,match_hostname,origin_scheme,origin_address,created_by_actor_id,updated_by_actor_id) VALUES($1,$2,'web','http','exact',$3,'http','127.0.0.1:3000','x','x')`, "tr_"+name+suffix, "tn_"+name+suffix, host)
		exec(`INSERT INTO paperboat.tunnel_domains(id,account_id,tunnel_id,route_id,hostname,match_type,ownership_challenge_reference,ownership_state,dns_target) VALUES($1,$2,$3,$4,$5,'exact','ref','verified','target.example.test')`, "td_"+name+suffix, map[string]string{"banned": owner, "free": other}[name], "tn_"+name+suffix, "tr_"+name+suffix, host)
	}
	exec(`INSERT INTO paperboat.bans(id,subject_kind,subject_id,reason,banned_by) VALUES('ban_u_'||$1,'user',$2,'case','operator_x'),('ban_t_'||$1,'team',$3,'case','operator_x')`, suffix, owner, team)
	exec(`INSERT INTO paperboat.bans(id,subject_kind,subject_id,reason,banned_by,lifted_at,lifted_by) VALUES('ban_l_'||$1,'user',$2,'case','operator_x',now(),'operator_x')`, suffix, other)

	decision := func(host string) wire.IngressDecision {
		var d wire.IngressDecision
		d.Binding.Hostname = host
		return d
	}
	// This edge holds decisions for a banned name, a free one and a lifted one: only the banned one is withheld.
	decisions := []wire.IngressDecision{decision(hosts["banned"]), decision(hosts["free"]), decision(hosts["lifted"]), decision(hosts["team"])}
	var denied []wire.IngressQuotaDenial
	blocked, err := withholdBlocked(ctx, store, &decisions, &denied)
	if err != nil {
		t.Fatal(err)
	}
	if len(decisions) != 2 || decisions[0].Binding.Hostname != hosts["free"] || decisions[1].Binding.Hostname != hosts["lifted"] {
		t.Fatalf("decisions left: %v", decisions)
	}
	got := map[string]bool{}
	for _, b := range blocked {
		got[b.Hostname] = b.Legal
	}
	if _, ok := got[hosts["banned"]]; !ok || got[hosts["banned"]] {
		t.Fatalf("a banned account's tunnel is not listed as a plain takedown: %v", got)
	}
	if _, ok := got[custom["banned"]]; !ok {
		t.Fatalf("a banned account's tunnel custom domain is not listed: %v", got)
	}
	if _, ok := got[custom["free"]]; ok {
		t.Fatalf("another owner's custom domain is listed: %v", got)
	}
	if _, ok := got[hosts["team"]]; !ok {
		t.Fatalf("a banned team's tunnel is not listed: %v", got)
	}
	for _, name := range []string{"free", "lifted", "removed"} {
		if _, ok := got[hosts[name]]; ok {
			t.Fatalf("%s is listed: %v", name, got)
		}
	}
}

// The takedown list never passes the edge's bound (the edge refuses a longer one and keeps serving its last snapshot).
func TestCapBlockedKeepsTheEdgesBound(t *testing.T) {
	long := make([]wire.IngressBlock, maximumBlocked+500)
	for i := range long {
		long[i].Hostname = fmt.Sprintf("h%d.example.test", i)
	}
	got := capBlocked(long)
	if len(got) != maximumBlocked || got[0].Hostname != "h0.example.test" {
		t.Fatalf("capped to %d", len(got))
	}
	if short := capBlocked(long[:10]); len(short) != 10 {
		t.Fatalf("a short list was cut to %d", len(short))
	}
}
