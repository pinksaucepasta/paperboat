package planlimits

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

func testStore(t *testing.T) *db.DB {
	t.Helper()
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
	t.Cleanup(func() { store.Close() })
	if err := db.Migrate(context.Background(), store); err != nil {
		t.Fatal(err)
	}
	return store
}

func check(t *testing.T, store *db.DB, scope Scope, kind Kind) error {
	t.Helper()
	var out error
	if err := store.InTx(context.Background(), func(ctx context.Context, tx *db.Tx) error {
		out = CheckTx(ctx, tx, scope, kind)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

func refused(t *testing.T, err error, plan Plan, limit int) {
	t.Helper()
	var l *LimitError
	if !errors.As(err, &l) || l.Plan != plan || l.Limit != limit || l.Code() == "" {
		t.Fatalf("want a %s refusal at %d, got %v", plan, limit, err)
	}
}

// Machines at the user-decided defaults: Free 100 (the 100th is admitted, the 101st refused),
// revoked and deleted never count, Pro 1,000, a team only meets its hidden ceiling.
func TestMachineLimitsAtTheirBoundariesPostgres(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	Configure(config.Default().Billing.Limits, "https://console.example.test/billing")
	x := fmt.Sprint(time.Now().UnixNano())
	user, team := "pl_u_"+x, "pl-t-"+x
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = store.SQL().ExecContext(bg, `DELETE FROM paperboat.user_machines WHERE user_id=$1`, user)
		_, _ = store.SQL().ExecContext(bg, `DELETE FROM paperboat.billing_workspaces WHERE user_id=$1`, user)
		_, _ = store.SQL().ExecContext(bg, `DELETE FROM paperboat.teams WHERE team_id=$1`, team)
		_, _ = store.SQL().ExecContext(bg, `DELETE FROM paperboat.users WHERE id=$1`, user)
	})
	exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@example.test','active')`, user)
	machines := func(from, to int, extra string) {
		t.Helper()
		exec(`INSERT INTO paperboat.user_machines(id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state`+extra+`)
 SELECT $1||i,$2,$1||i,'pl-'||i,'linux','amd64','/w','online','occupied'`+map[string]string{"": "", ",revoked_at": ",now()", ",owner_team_id": ",'" + team + "'"}[extra]+` FROM generate_series($3::int,$4::int) i`, "pl_m_"+x+"_", user, from, to)
	}
	personal := Scope{Account: user}
	machines(1, 99, "")
	if err := check(t, store, personal, Machines); err != nil {
		t.Fatalf("99 machines, the 100th must be admitted: %v", err)
	}
	machines(100, 100, "")
	refused(t, check(t, store, personal, Machines), Free, 100)
	machines(101, 140, ",revoked_at")
	exec(`UPDATE paperboat.user_machines SET deleted_at=now() WHERE id=$1`, "pl_m_"+x+"_100")
	if err := check(t, store, personal, Machines); err != nil {
		t.Fatalf("revoked and deleted machines counted: %v", err)
	}
	machines(150, 150, "")
	refused(t, check(t, store, personal, Machines), Free, 100)
	exec(`INSERT INTO paperboat.billing_workspaces(id,user_id,provider_customer_id,provider_subscription_id,product_code,state,quantity,period_start,period_end) VALUES($1,$2,$3,$4,'pro-month','active',1,now()-interval '1 day',now()+interval '29 days')`, "personal:"+user, user, "cus_pl_"+x, "sub_pl_"+x)
	if err := check(t, store, personal, Machines); err != nil {
		t.Fatalf("Pro must allow more than 100: %v", err)
	}
	exec(`INSERT INTO paperboat.teams(team_id,owner_account,generation) VALUES($1,$2,1)`, team, user)
	machines(200, 349, ",owner_team_id")
	if err := check(t, store, Scope{Team: team}, Machines); err != nil {
		t.Fatalf("a team with 150 machines is far below its ceiling: %v", err)
	}
	var usage map[Kind]Usage
	if err := store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		var err error
		usage, err = UsageTx(ctx, tx, Scope{Team: team})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if u := usage[Machines]; u.Used != 150 || u.Limit != nil {
		t.Fatalf("team usage %+v, want 150 used and no advertised limit", u)
	}
	limits := config.Default().Billing.Limits
	limits.Machines.TeamCeiling = 150
	Configure(limits, "")
	t.Cleanup(func() { Configure(config.Default().Billing.Limits, "") })
	refused(t, check(t, store, Scope{Team: team}, Machines), Team, 150)
}

// Previews and tunnels count together, only while active; open browser terminals count distinct
// terminal sessions, so a reconnect or another viewer of an open terminal is always admitted.
func TestPublicationAndBrowserTerminalLimitsPostgres(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	limits := config.Default().Billing.Limits
	limits.Publications.Free, limits.BrowserTerminals.Free = 2, 1
	Configure(limits, "")
	t.Cleanup(func() { Configure(config.Default().Billing.Limits, "") })
	x := fmt.Sprint(time.Now().UnixNano())
	user, machine := "pl_pu_"+x, "pl_pm_"+x
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		bg := context.Background()
		for _, q := range []string{`DELETE FROM paperboat.native_usage_authorities WHERE machine_id=$1`, `DELETE FROM paperboat.preview_leases WHERE owner_machine_id=$1`, `DELETE FROM paperboat.tunnels WHERE created_by_host_id=$1`, `DELETE FROM paperboat.user_machines WHERE id=$1`} {
			_, _ = store.SQL().ExecContext(bg, q, machine)
		}
		_, _ = store.SQL().ExecContext(bg, `DELETE FROM paperboat.users WHERE id=$1`, user)
	})
	exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@example.test','active')`, user)
	exec(`INSERT INTO paperboat.user_machines(id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state) VALUES($1,$2,$1,'pl-pub','linux','amd64','/w','online','occupied')`, machine, user)
	exec(`UPDATE paperboat.user_machines SET online=true WHERE id=$1`, machine)
	personal := Scope{Account: user}
	preview := func(id, state string) {
		t.Helper()
		exec(`INSERT INTO paperboat.preview_leases(id,endpoint,account_id,actor_id,owner_machine_id,owner_session_id,target_scheme,target_address,lease_deadline,owner_session_kind,terminal_state,stopped_at)
 VALUES($1,$5,$2,$2,$3,'s','http','127.0.0.1:1',now()+interval '1 hour','foreground',$4,CASE WHEN $4='active' THEN NULL ELSE now() END)`, id, user, machine, state, "https://plpv"+fmt.Sprint(len(id))+"-"+x+".preview.example.test")
	}
	preview("pl_pv_stopped_"+x, "stopped")
	preview("pl_pv_live_"+x, "active")
	if err := check(t, store, personal, Publications); err != nil {
		t.Fatalf("one active preview of 2: %v", err)
	}
	exec(`INSERT INTO paperboat.tunnels(id,account_id,name,stable_endpoint_id,stable_endpoint,created_by_host_id,created_by_actor_id,desired_state) VALUES($1,$2,'t',$5,$4,$3,$2,'active')`, "pl_tn_"+x, user, machine, "https://pltn-"+x+".tunnel.example.test", "endpoint_"+uuid.NewString())
	refused(t, check(t, store, personal, Publications), Free, 2)
	// Only live counts (user rule): with the machine offline its preview and tunnel serve nothing.
	exec(`UPDATE paperboat.user_machines SET online=false,state='offline' WHERE id=$1`, machine)
	if err := check(t, store, personal, Publications); err != nil {
		t.Fatalf("publications of an offline machine counted: %v", err)
	}
	exec(`UPDATE paperboat.user_machines SET online=true,state='online' WHERE id=$1`, machine)
	refused(t, check(t, store, personal, Publications), Free, 2)
	exec(`UPDATE paperboat.tunnels SET desired_state='deleted',deleted_at=now() WHERE id=$1`, "pl_tn_"+x)
	if err := check(t, store, personal, Publications); err != nil {
		t.Fatalf("a deleted tunnel counted: %v", err)
	}
	admit := func(attachment, terminal string) {
		t.Helper()
		exec(`INSERT INTO paperboat.native_usage_authorities(access_session_id,machine_id,installation_generation,workspace_id,resource_kind,resource_id,resource_name,machine_name,capabilities,admitted_at,expires_at,retain_until,edge_node_id)
 VALUES($1,$2,1,$3,'terminal',$4,'t','pl-pub',ARRAY['terminal'],now(),now()+interval '5 minutes',now()+interval '7 days','node_pl')`, attachment, machine, "personal:"+user, terminal)
	}
	browser := func(terminal string) error {
		t.Helper()
		var out error
		if err := store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
			out = CheckBrowserTerminalTx(ctx, tx, personal, terminal)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		return out
	}
	if err := browser("term_a_" + x); err != nil {
		t.Fatalf("first browser terminal: %v", err)
	}
	admit("att_a1_"+x, "term_a_"+x)
	if err := browser("term_a_" + x); err != nil {
		t.Fatalf("reconnect to the open terminal: %v", err)
	}
	refused(t, browser("term_b_"+x), Free, 1)
}

// Public raw-TCP tunnels (user decision 2026-10-27): Free 1, Pro 10 per account, a team 10 per paid seat
// pooled; only live ones count and a workspace on a self-hosted edge is not limited.
func TestPublicTCPLimitsPostgres(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	Configure(config.Default().Billing.Limits, "https://console.example.test/billing")
	t.Cleanup(func() { Configure(config.Default().Billing.Limits, "") })
	x := fmt.Sprint(time.Now().UnixNano())
	user, other, team, machine := "pl_tu_"+x, "pl_to_"+x, "pl-tt-"+x, "pl_tm_"+x
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		bg := context.Background()
		for _, c := range []struct {
			query string
			arg   string
		}{
			{`DELETE FROM paperboat.tunnel_routes WHERE tunnel_id LIKE $1`, "pl_tt_" + x + "_%"},
			{`DELETE FROM paperboat.tunnels WHERE created_by_host_id=$1`, machine},
			{`DELETE FROM paperboat.selfhost_pool_policies WHERE scope_id=$1`, user},
			{`DELETE FROM paperboat.billing_workspaces WHERE team_id=$1`, team},
			{`DELETE FROM paperboat.billing_workspaces WHERE user_id=$1`, user},
			{`DELETE FROM paperboat.teams WHERE team_id=$1`, team},
			{`DELETE FROM paperboat.user_machines WHERE id=$1`, machine},
			{`DELETE FROM paperboat.users WHERE id=$1 OR id=$2`, user},
		} {
			args := []any{c.arg}
			if strings.Contains(c.query, "$2") {
				args = append(args, other)
			}
			_, _ = store.SQL().ExecContext(bg, c.query, args...)
		}
	})
	exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@example.test','active'),($2,$2,$2||'@example.test','active')`, user, other)
	exec(`INSERT INTO paperboat.user_machines(id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state,online) VALUES($1,$2,$1,'pl-tcp','linux','amd64','/w','online','occupied',true)`, machine, user)
	port := 20000 + int(time.Now().UnixNano()%7000)
	next := 0
	// tunnelTCP adds one tunnel with a public TCP route, owned by the team when team is set.
	tunnelTCP := func(team string) string {
		t.Helper()
		next++
		id := fmt.Sprintf("pl_tt_%s_%d", x, next)
		exec(`INSERT INTO paperboat.tunnels(id,account_id,name,stable_endpoint_id,stable_endpoint,created_by_host_id,created_by_actor_id,desired_state,owner_team_id)
 VALUES($1,$2,$1,'endpoint_'||gen_random_uuid()::text,'https://'||replace($1,'_','-')||'.example.test',$3,$2,'active',NULLIF($4,''))`, id, user, machine, team)
		exec(`INSERT INTO paperboat.tunnel_routes(id,tunnel_id,name,protocol,match_type,match_hostname,origin_scheme,origin_address,preserve_host,created_by_actor_id,updated_by_actor_id,public_tcp_listener_id,public_tcp_port)
 VALUES('rte_'||$1,$1,'default','tcp','managed',replace($1,'_','-')||'.example.test','tcp','127.0.0.1:1',false,$2,$2,'listener_'||$1,$3)`, id, user, port+next)
		return id
	}
	personal := Scope{Account: user}
	if err := check(t, store, personal, PublicTCP); err != nil {
		t.Fatalf("the first public TCP tunnel on Free: %v", err)
	}
	first := tunnelTCP("")
	refused(t, check(t, store, personal, PublicTCP), Free, 1)
	var refusal *LimitError
	if !errors.As(check(t, store, personal, PublicTCP), &refusal) || refusal.Code() != "public_tcp_limit_reached" || refusal.BillingURL == "" {
		t.Fatalf("refusal %+v", refusal)
	}
	// Only live ones count: an offline machine, a deleted tunnel and a deleted route serve nothing.
	exec(`UPDATE paperboat.user_machines SET online=false WHERE id=$1`, machine)
	if err := check(t, store, personal, PublicTCP); err != nil {
		t.Fatalf("a tunnel of an offline machine counted: %v", err)
	}
	exec(`UPDATE paperboat.user_machines SET online=true WHERE id=$1`, machine)
	exec(`UPDATE paperboat.tunnel_routes SET desired_state='deleted',deleted_at=now() WHERE tunnel_id=$1`, first)
	if err := check(t, store, personal, PublicTCP); err != nil {
		t.Fatalf("a deleted route counted: %v", err)
	}
	// Self-hosted edge: unlimited, not counted.
	tunnelTCP("")
	refused(t, check(t, store, personal, PublicTCP), Free, 1)
	exec(`INSERT INTO paperboat.selfhost_pool_policies(capability,mode,scope_kind,scope_id,installation_ids) VALUES('tunnel','self-hosted-only','account',$1,'{}')`, user)
	if err := check(t, store, personal, PublicTCP); err != nil {
		t.Fatalf("a self-hosted workspace was limited: %v", err)
	}
	exec(`DELETE FROM paperboat.selfhost_pool_policies WHERE scope_id=$1`, user)
	// Pro: 10 per account.
	exec(`INSERT INTO paperboat.billing_workspaces(id,user_id,provider_customer_id,provider_subscription_id,product_code,state,quantity,period_start,period_end) VALUES($1,$2,$3,$4,'pro-month','active',1,now()-interval '1 hour',now()+interval '1 hour')`, "bw_p_"+x, user, "cus_"+x, "sub_"+x)
	for i := 0; i < 8; i++ {
		tunnelTCP("")
	}
	if err := check(t, store, personal, PublicTCP); err != nil {
		t.Fatalf("9 of 10 on Pro: %v", err)
	}
	tunnelTCP("")
	refused(t, check(t, store, personal, PublicTCP), Pro, 10)
	// Team: 10 per paid seat, pooled; the personal tunnels above are not the team's.
	exec(`INSERT INTO paperboat.teams(team_id,owner_account,generation) VALUES($1,$2,1)`, team, user)
	exec(`INSERT INTO paperboat.billing_workspaces(id,team_id,state,product_code,quantity,period_start,period_end) VALUES($1,$2,'active','team-pro',2,now()-interval '1 hour',now()+interval '1 hour')`, "bw_t_"+x, team)
	teamScope := Scope{Account: user, Team: team}
	for i := 0; i < 19; i++ {
		tunnelTCP(team)
	}
	if err := check(t, store, teamScope, PublicTCP); err != nil {
		t.Fatalf("19 of 20 on a two-seat team: %v", err)
	}
	tunnelTCP(team)
	refused(t, check(t, store, teamScope, PublicTCP), Team, 20)
}
