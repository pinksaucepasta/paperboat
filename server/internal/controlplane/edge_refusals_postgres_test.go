package controlplane

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/bandwidthgate"
	wire "github.com/pinksaucepasta/paperboat/shared/connectorprotocol"
	"github.com/pinksaucepasta/paperboat/shared/edgelimits"
)

// A heartbeat's flood-protection refusals add up per hostname, edge and hour; malformed entries are
// skipped, and counts past retention are deleted by the same writer.
func TestEdgeRefusalsAccumulateHourlyAndExpire(t *testing.T) {
	t.Setenv("PGTZ", "Asia/Kolkata") // a half-hour offset session zone must still bucket by UTC hour
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

// A taken-down hostname's decisions and throttles are withheld from the edge snapshot and the
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
	decisions := []wire.IngressDecision{decision(taken), decision(open), decision(restored), decision(quota)}
	throttled := []wire.IngressThrottle{{Hostname: quota}}
	blocked, err := withholdBlocked(ctx, store, &decisions, &throttled)
	if err != nil {
		t.Fatal(err)
	}
	if len(decisions) != 2 || decisions[0].Binding.Hostname != open || decisions[1].Binding.Hostname != restored || len(throttled) != 0 {
		t.Fatalf("decisions=%v throttled=%v", decisions, throttled)
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
	owner, machine, payer := "user_rules_"+suffix, "host_rules_"+suffix, "user_pro_"+suffix
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
		_, _ = store.SQL().ExecContext(cleanup, `DELETE FROM paperboat.billing_workspaces WHERE user_id=$1`, payer)
		_, _ = store.SQL().ExecContext(cleanup, `DELETE FROM paperboat.users WHERE id = ANY($1)`, []string{owner, payer})
	})
	exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,email_verified,status) VALUES($1,$1,'owner@test.invalid',false,'active'),($2,$2,'payer@test.invalid',false,'active')`, owner, payer)
	exec(`INSERT INTO paperboat.billing_workspaces(id,user_id,provider_customer_id,provider_subscription_id,product_code,state,quantity,period_start,period_end) VALUES($1,$2,$3,$4,'pro-month','active',1,now()-interval '1 day',now()+interval '29 days')`, "personal:"+payer, payer, "cus_"+suffix, "sub_"+suffix)
	exec(`INSERT INTO paperboat.user_machines(id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state) VALUES($1,$2,$3,'rules-host','linux','amd64','/workspace','offline','released')`, machine, owner, "env_rules_"+suffix)
	for i, tunnel := range []string{ruled, other} {
		endpoint := fmt.Sprintf("%08d-0000-4000-8000-%s", i, suffix[len(suffix)-12:])
		exec(`INSERT INTO paperboat.tunnels(id,account_id,name,desired_state,access_mode,generation,stable_endpoint_id,stable_endpoint,created_by_host_id,created_by_actor_id,created_at,updated_at) VALUES($1,$2,$3,'active','public',1,$4,$5,$6,$2,$7,$7)`, tunnel, map[bool]string{false: owner, true: payer}[tunnel == other], "rules-"+tunnel, "endpoint_"+endpoint, "https://"+endpoint+".tunnels.example.test", machine, now)
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
	// Free-owned publications are marked for the edge's request rates; the tunnel of a paid personal plan is not (and, with no
	// lists, is not listed at all).
	if !got["a.example.test"].FreeRequests || !got["b.example.test"].FreeRequests || got["p.example.test"].FreeRequests {
		t.Fatalf("free marks = %+v", rules)
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
	var throttled []wire.IngressThrottle
	blocked, err := withholdBlocked(ctx, store, &decisions, &throttled)
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

// The byte pacing, the Free request rates, the browser-terminal cap and the fair-use file throttles are Paperboat's limits on hosted traffic: a
// Paperboat-run edge (a plain node, or a global installation) is sent them, a customer-owned edge (account or team
// installation) never is, so the same Free publication is limited on the one and unlimited on the other.
func TestHostedLimitsReachPaperboatEdgesAndNeverCustomerOwnedOnesPostgres(t *testing.T) {
	store := openControlPlaneTestDB(t)
	ctx := t.Context()
	x := fmt.Sprint(time.Now().UnixNano())
	user := "hl_user_" + x
	nodes := map[string]string{"plain": "hl_plain_" + x, "global": "hl_global_" + x, "account": "hl_account_" + x, "team": "hl_team_" + x}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = store.SQL().ExecContext(bg, `DELETE FROM native_usage_authorities WHERE workspace_id='personal:'||$1`, user)
		_, _ = store.SQL().ExecContext(bg, `DELETE FROM control_routes WHERE applied_node_id = ANY($1)`, []string{nodes["plain"], nodes["global"], nodes["account"], nodes["team"]})
		_, _ = store.SQL().ExecContext(bg, `DELETE FROM control_environments WHERE id LIKE 'hl_env_%'||$1`, x)
		_, _ = store.SQL().ExecContext(bg, `DELETE FROM user_machines WHERE user_id=$1`, user)
		_, _ = store.SQL().ExecContext(bg, `DELETE FROM billing_workspace_usage_periods WHERE workspace_id='personal:'||$1`, user)
		_, _ = store.SQL().ExecContext(bg, `DELETE FROM billing_workspaces WHERE id='personal:'||$1`, user)
		_, _ = store.SQL().ExecContext(bg, `DELETE FROM users WHERE id=$1`, user)
		for _, n := range nodes {
			_, _ = store.SQL().ExecContext(bg, `DELETE FROM selfhost_installations WHERE node_id=$1`, n)
			_, _ = store.SQL().ExecContext(bg, `DELETE FROM control_tunnel_nodes WHERE id=$1`, n)
		}
	})
	exec(`INSERT INTO users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@example.test','active')`, user)
	exec(`INSERT INTO billing_workspaces(id,user_id,state) VALUES('personal:'||$1,$1,'free')`, user)
	now := time.Now().UTC()
	// The workspace is past its included traffic, so its terminal file transfers are throttled on a hosted edge.
	exec(`INSERT INTO billing_workspace_usage_periods(workspace_id,period_start,period_end,included_bytes,consumed_bytes,paid,budget_start) VALUES('personal:'||$1,$2,$3,100,100,false,$2)`, user, now.Add(-time.Hour), now.AddDate(0, 1, 0))
	scopes := map[string][2]string{"global": {"global", "operator"}, "account": {"account", user}, "team": {"team", "hl_team_" + x}}
	for kind, node := range nodes {
		exec(`INSERT INTO control_tunnel_nodes(id,edge_pool,protocol_version,process_epoch) VALUES($1,'default','1.0',$1)`, node)
		if scope, ok := scopes[kind]; ok {
			key := sha256.Sum256([]byte(node)) // runtime_hash is unique per installation
			exec(`INSERT INTO selfhost_installations(id,node_id,installation_public_key,runtime_hash,runtime_ciphertext,name,capability,scope_kind,scope_id,claim_id,claim_complete)
 VALUES($1,$1,$2,$2,'fixture','hosted limits fixture','tunnel',$3,$4,$1,true)`, node, key[:], scope[0], scope[1])
		}
		machine, env := "hl_m_"+kind+x, "hl_env_"+kind+x
		exec(`INSERT INTO user_machines(id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state,online) VALUES($1,$2,$3,'hl'||$4,'linux','amd64','/w','online','occupied',true)`, machine, user, env, kind)
		exec(`INSERT INTO control_environments(id,workspace_id) VALUES($1,$2)`, env, "ws_"+kind+x)
		exec(`INSERT INTO control_routes(id,environment_id,kind,public_host,target_host,target_port,applied_node_id) VALUES($1,$2,'runtime_https_wss',$3,'127.0.0.1',8443,$4)`, "hl_r_"+kind+x, env, "hl-"+kind+x+".runtime.example.test", node)
		exec(`INSERT INTO native_usage_authorities(access_session_id,machine_id,installation_generation,workspace_id,resource_kind,resource_id,resource_name,machine_name,capabilities,admitted_at,expires_at,retain_until,edge_node_id,attached_until)
 VALUES($1,$2,1,'personal:'||$3,'terminal','term','t','hl',ARRAY['terminal'],now(),now()+interval '5 minutes',now()+interval '7 days',$4,now()+interval '1 minute')`, "hl_att_"+kind+x, machine, user, node)
	}
	service := NewEdgeService(store, "hosted-limits-test-credential-0123456789")
	gate := bandwidthgate.New(store)
	gate.SetLookupTimeout(5 * time.Second) // this test database is slower than production
	service.workspaceGate = gate
	for kind, node := range nodes {
		customerOwned := kind == "account" || kind == "team"
		limits := service.ingressLimitsFor(ctx, node)
		if limits.Validate() != nil {
			t.Errorf("%s edge: invalid set %+v", kind, limits)
		}
		// A Paperboat edge is sent every limit; a customer-owned one is sent the zero set: no byte pacing, no Free request rates.
		if customerOwned && limits != (edgelimits.IngressLimits{}) {
			t.Errorf("%s edge was sent limits: %+v", kind, limits)
		}
		if !customerOwned && (limits.Publication == (edgelimits.Limit{}) || limits.Account == (edgelimits.Limit{}) || limits.FreeRequests == (edgelimits.FreeRequests{})) {
			t.Errorf("%s edge was not sent every limit: %+v", kind, limits)
		}
		_, throttled, _, _, _, err := service.ingressProjection(ctx, node, "epoch_"+x+"_hostedlimits", true)
		if err != nil {
			t.Fatal(err)
		}
		terminal, files := 0, 0
		for _, entry := range throttled {
			if entry.Terminal {
				terminal++
			} else if strings.HasPrefix(entry.Hostname, "files:") {
				files++
			}
		}
		if want := map[bool]int{true: 0, false: 1}[customerOwned]; terminal != want || files != want {
			t.Errorf("%s edge: %d terminal caps and %d file throttles listed, want %d of each", kind, terminal, files, want)
		}
	}
}
