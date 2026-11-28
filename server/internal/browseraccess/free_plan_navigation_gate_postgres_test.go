package browseraccess

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

// On the Free plan a public HTTP tunnel route stays public at the edge (only page loads sign in): its viewer policy may
// name who signs in but never "public"; Pro, a Team, and the operator's public_browser grant keep it fully public.
// A "public" policy from before the gate means the owner on Free and is public again after an upgrade.
func TestFreePlanNavigationGatePostgres(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("PAPERBOAT_TEST_DATABASE_DSN"))
	if dsn == "" {
		t.Skip("set PAPERBOAT_TEST_DATABASE_DSN")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	store, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	if os.Getenv("PAPERBOAT_TEST_SCHEMA_READY") != "1" {
		if err = db.Migrate(ctx, store); err != nil {
			t.Fatal(err)
		}
	}
	suffix := fmt.Sprint(time.Now().UnixNano())
	now := time.Now().UTC().Truncate(time.Microsecond)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, e := store.SQL().ExecContext(ctx, q, args...); e != nil {
			t.Fatal(e)
		}
	}
	type fixture struct{ owner, machine, tunnel, route, host string }
	var made []fixture
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		for _, f := range made {
			_, _ = store.SQL().ExecContext(c, `DELETE FROM paperboat.browser_viewer_policies WHERE resource_id=$1`, f.tunnel)
			_, _ = store.SQL().ExecContext(c, `DELETE FROM paperboat.tunnels WHERE id=$1`, f.tunnel)
			_, _ = store.SQL().ExecContext(c, `DELETE FROM paperboat.user_machines WHERE id=$1`, f.machine)
			_, _ = store.SQL().ExecContext(c, `DELETE FROM paperboat.billing_workspace_grants WHERE workspace_id='personal:'||$1`, f.owner)
			_, _ = store.SQL().ExecContext(c, `DELETE FROM paperboat.billing_workspaces WHERE id='personal:'||$1`, f.owner)
			_, _ = store.SQL().ExecContext(c, `DELETE FROM paperboat.users WHERE id=$1`, f.owner)
		}
	})
	mk := func(name string, pro bool) fixture {
		t.Helper()
		f := fixture{owner: "usr_ng_" + name + "_" + suffix, machine: "host_ng_" + name + "_" + suffix, tunnel: "tun_ng_" + name + "_" + suffix, route: "rte_ng_" + name + "_" + suffix}
		endpointID := fmt.Sprintf("00000000-0000-4000-8%03d-%s", len(made), suffix[len(suffix)-12:])
		f.host = endpointID + ".tunnels.example.test"
		made = append(made, f)
		exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,email_verified,status) VALUES($1,$1,$1||'@test.invalid',true,'active')`, f.owner)
		if pro {
			exec(`INSERT INTO paperboat.billing_workspaces(id,user_id,product_code,state,provider_subscription_id,period_start,period_end) VALUES('personal:'||$1,$1,'pro-monthly','active','sub_'||$1,now()-interval '1 day',now()+interval '1 day')`, f.owner)
		}
		exec(`INSERT INTO paperboat.user_machines(id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state) VALUES($1,$2,$3,'ng-host','linux','amd64','/workspace','offline','released')`, f.machine, f.owner, "env_"+f.machine)
		exec(`INSERT INTO paperboat.tunnels(id,account_id,name,desired_state,access_mode,generation,stable_endpoint_id,stable_endpoint,created_by_host_id,created_by_actor_id,created_at,updated_at) VALUES($1,$2,'nav-gate','active','public',1,$3,$4,$5,$2,$6,$6)`, f.tunnel, f.owner, "endpoint_"+endpointID, "https://"+f.host, f.machine, now)
		exec(`INSERT INTO paperboat.tunnel_routes(id,tunnel_id,name,protocol,match_type,match_hostname,path_prefix,priority,origin_scheme,origin_address,desired_state,generation,created_by_actor_id,updated_by_actor_id,created_at,updated_at) VALUES($1,$2,'a','http','managed',$3,'/',100,'http','127.0.0.1:3100','active',1,$4,$4,$5,$5)`, f.route, f.tunnel, f.host, f.owner, now)
		return f
	}
	svc, err := NewService(store, "https://login.example.test")
	if err != nil {
		t.Fatal(err)
	}
	svc.now = func() time.Time { return now }
	set := func(f fixture, audience string, generation int64, emails ...string) (ViewerPolicy, error) {
		return svc.SetViewerPolicy(ctx, SetViewerPolicyRequest{ActorAccountID: f.owner, ResourceKind: "tunnel", ResourceID: f.tunnel, RouteID: f.route, Audience: audience, AllowedEmails: emails, ExpectedGeneration: generation})
	}
	effective := func(f fixture) string {
		t.Helper()
		var mode string
		if e := store.SQL().QueryRowContext(ctx, `SELECT effective_access_mode FROM paperboat.current_tunnel_route_viewer_policy WHERE route_id=$1`, f.route).Scan(&mode); e != nil {
			t.Fatal(e)
		}
		return mode
	}
	gated := func(f fixture) (bool, string) {
		t.Helper()
		var a authority
		e := store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
			var err error
			a, err = resolveResource(ctx, tx, f.host, "tunnel", f.tunnel, f.route, now)
			return err
		})
		if e != nil {
			t.Fatal(e)
		}
		return a.gated, a.mode
	}

	free := mk("free", false)
	if g, mode := gated(free); !g || mode != "private" {
		t.Fatalf("free public route: gated=%v mode=%q, want a private browser grant", g, mode)
	}
	if p, e := svc.GetViewerPolicy(ctx, free.owner, "tunnel", free.tunnel, free.route); e != nil || !p.BrowserSignInRequired {
		t.Fatalf("free policy read: %+v %v", p, e)
	}
	if _, e := set(free, AudiencePublic, 0); !errors.Is(e, ErrPlanBrowserPublic) {
		t.Fatalf("free public audience: %v, want ErrPlanBrowserPublic", e)
	}
	p, e := set(free, AudienceSpecificEmails, 0, "friend@company.test")
	if e != nil || !p.BrowserSignInRequired {
		t.Fatalf("free email allowlist: %+v %v", p, e)
	}
	// The edge keeps serving it as public; only page loads are gated (by the snapshot's navigation mark).
	if m := effective(free); m != "public" {
		t.Fatalf("free route with an allowlist: edge mode %q, want public", m)
	}

	pro := mk("pro", true)
	if g, mode := gated(pro); g || mode != "public" {
		t.Fatalf("pro public route: gated=%v mode=%q", g, mode)
	}
	if p, e := set(pro, AudiencePublic, 0); e != nil || p.BrowserSignInRequired {
		t.Fatalf("pro public audience: %+v %v", p, e)
	}
	if _, e := set(pro, AudienceOwner, 1); e != nil || effective(pro) != "private" {
		t.Fatalf("pro owner audience: %v, edge mode %q, want private as before", e, effective(pro))
	}

	// An operator's public_browser grant lifts the gate at once; revoking it brings it back.
	lifted := mk("grant", false)
	exec(`INSERT INTO paperboat.billing_workspace_grants(id,workspace_id,kind,amount,reason,created_by) VALUES($1,'personal:'||$2,'public_browser',1,'test','operator')`, "grant_"+lifted.owner, lifted.owner)
	if g, _ := gated(lifted); g {
		t.Fatal("public_browser grant did not lift the gate")
	}
	if _, e := set(lifted, AudiencePublic, 0); e != nil {
		t.Fatalf("granted public audience: %v", e)
	}
	exec(`UPDATE paperboat.billing_workspace_grants SET revoked_at=now(),revoked_by='operator' WHERE id=$1`, "grant_"+lifted.owner)
	if g, _ := gated(lifted); !g {
		t.Fatal("revoked grant still lifts the gate")
	}

	// Free, placed on the owner's own self-hosted edge: exempt, fully public (a policy may say "public").
	selfHosted := mk("selfhost", false)
	node := "node_ng_" + suffix
	exec(`INSERT INTO paperboat.control_tunnel_nodes(id,edge_pool,protocol_version,process_epoch) VALUES($1,'default','1.0',$1)`, node)
	exec(`INSERT INTO paperboat.selfhost_installations(id,node_id,installation_public_key,runtime_hash,runtime_ciphertext,name,capability,scope_kind,scope_id,claim_id,claim_complete) VALUES($1,$1,decode(md5($1)||md5($1),'hex'),sha256(convert_to($1::text,'UTF8')),'encrypted','ng fixture','tunnel','account',$2,$1,true)`, node, selfHosted.owner)
	exec(`INSERT INTO paperboat.selfhost_placements(resource_kind,resource_id,installation_id) VALUES('tunnel',$1,$2)`, selfHosted.tunnel, node)
	t.Cleanup(func() {
		_, _ = store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.selfhost_placements WHERE resource_id=$1`, selfHosted.tunnel)
		_, _ = store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.selfhost_installations WHERE id=$1`, node)
		_, _ = store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.control_tunnel_nodes WHERE id=$1`, node)
	})
	if g, mode := gated(selfHosted); g || mode != "public" {
		t.Fatalf("free self-hosted route: gated=%v mode=%q, want fully public", g, mode)
	}
	if p, e := set(selfHosted, AudiencePublic, 0); e != nil || p.BrowserSignInRequired {
		t.Fatalf("free self-hosted public audience: %+v %v", p, e)
	}
	// Free on a managed edge stays owner-only for page loads (the plain free fixture above): no public policy.
	if g, _ := gated(free); !g {
		t.Fatal("free managed route not gated")
	}

	// Paid on a self-hosted edge: unchanged, public allowed.
	paidSelf := mk("paidself", true)
	exec(`INSERT INTO paperboat.selfhost_placements(resource_kind,resource_id,installation_id) VALUES('tunnel',$1,$2)`, paidSelf.tunnel, node)
	t.Cleanup(func() {
		_, _ = store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.selfhost_placements WHERE resource_id=$1`, paidSelf.tunnel)
	})
	if g, mode := gated(paidSelf); g || mode != "public" {
		t.Fatalf("paid self-hosted route: gated=%v mode=%q", g, mode)
	}
	if _, e := set(paidSelf, AudiencePublic, 0); e != nil {
		t.Fatalf("paid self-hosted public audience: %v", e)
	}
	// Native private (and team) modes are never touched by the gate, on Free too.
	for _, native := range []string{"private", "team"} {
		f := mk("native"+native, false)
		exec(`UPDATE paperboat.tunnels SET access_mode=$2 WHERE id=$1`, f.tunnel, native)
		if g, mode := gated(f); g || mode != native {
			t.Fatalf("free native %s route: gated=%v mode=%q", native, g, mode)
		}
		if m := effective(f); m != native {
			t.Fatalf("free native %s route: edge mode %q", native, m)
		}
	}

	// A Team workspace is never gated.
	var teamGated bool
	if e := store.SQL().QueryRowContext(ctx, `SELECT paperboat.workspace_navigation_gated('tunnel','tun_any',$1,'team_any')`, free.owner).Scan(&teamGated); e != nil || teamGated {
		t.Fatalf("team workspace gated=%v err=%v", teamGated, e)
	}

	// A "public" policy written before the gate is not rewritten: on Free it signs in as the owner only (no browser
	// grant for anyone else), and once the gate lifts (Pro, the grant, a self-hosted edge) the route is public again.
	stale := mk("stale", false)
	exec(`INSERT INTO paperboat.browser_viewer_policies(resource_kind,resource_id,route_id,owner_account_id,audience,allowed_emails,generation,updated_by_account,created_at,updated_at) VALUES('tunnel',$1,$2,$3,'public','{}',4,$3,$4,$4)`, stale.tunnel, stale.route, stale.owner, now)
	if g, mode := gated(stale); !g || mode != "private" || effective(stale) != "public" {
		t.Fatalf("free stale public policy: gated=%v mode=%q edge=%q", g, mode, effective(stale))
	}
	var viewerAudience string
	if e := store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		a, err := resolveResource(ctx, tx, stale.host, "tunnel", stale.tunnel, stale.route, now)
		if err != nil {
			return err
		}
		a, err = applyViewerPolicy(ctx, tx, a, stale.owner)
		viewerAudience = a.policyAudience
		return err
	}); e != nil || viewerAudience != AudienceOwner {
		t.Fatalf("free stale public policy grants the owner as %q: %v", viewerAudience, e)
	}
	exec(`INSERT INTO paperboat.billing_workspaces(id,user_id,product_code,state,provider_subscription_id,period_start,period_end) VALUES('personal:'||$1,$1,'pro-monthly','active','sub_'||$1,now()-interval '1 day',now()+interval '1 day')`, stale.owner)
	if g, mode := gated(stale); g || mode != "public" || effective(stale) != "public" {
		t.Fatalf("upgraded stale public policy: gated=%v mode=%q edge=%q, want public again", g, mode, effective(stale))
	}
}
