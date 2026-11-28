package browseraccess

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/testutil"
)

type privatePolicyFixture struct {
	t                                *testing.T
	ctx                              context.Context
	store                            *db.DB
	svc                              *Service
	suffix, owner, machine           string
	listed, domain, subdomain, other string
	unverified                       string
	users                            []string
	now                              time.Time
}

func newPrivatePolicyFixture(t *testing.T) *privatePolicyFixture {
	t.Helper()
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
	t.Cleanup(func() { store.Close() }) // the first cleanup runs last: later cleanups still use store
	if os.Getenv("PAPERBOAT_TEST_SCHEMA_READY") != "1" {
		if err = db.Migrate(ctx, store); err != nil {
			t.Fatal(err)
		}
	}
	svc, err := NewService(store, "https://login.example.test")
	if err != nil {
		t.Fatal(err)
	}
	suffix := fmt.Sprint(time.Now().UnixNano())
	f := &privatePolicyFixture{t: t, ctx: ctx, store: store, svc: svc, suffix: suffix, now: time.Now().UTC().Truncate(time.Microsecond),
		owner: "usr_pp_owner_" + suffix, machine: "mch_pp_" + suffix, listed: "usr_pp_listed_" + suffix, domain: "usr_pp_domain_" + suffix,
		subdomain: "usr_pp_sub_" + suffix, other: "usr_pp_other_" + suffix, unverified: "usr_pp_unverified_" + suffix}
	svc.now = func() time.Time { return f.now }
	t.Cleanup(func() {
		_, _ = store.SQL().ExecContext(ctx, `DELETE FROM paperboat.control_environments WHERE id=$1`, "env_pp_"+suffix)
		_, _ = store.SQL().ExecContext(ctx, `DELETE FROM paperboat.user_machines WHERE id=$1`, f.machine)
		for _, user := range f.users {
			_, _ = store.SQL().ExecContext(ctx, `DELETE FROM paperboat.users WHERE id=$1`, user)
		}
	})
	for _, user := range []struct {
		id, email string
		verified  bool
	}{
		{f.owner, "owner-" + suffix + "@owner.test", true},
		{f.listed, "Guest@Partner.test", true},
		{f.domain, "Anyone@Company.test", true},
		{f.subdomain, "anyone@sub.company.test", true},
		{f.other, "other@partner.test", true},
		{f.unverified, "second@company.test", false},
	} {
		f.exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,email_verified,status) VALUES($1,$1,$2,$3,'active')`, user.id, user.email, user.verified)
		f.exec(`INSERT INTO paperboat.sessions(id,user_id,session_hash,csrf_hash,expires_at) VALUES($1,$2,$3,$4,$5)`, "ses_"+user.id, user.id, "hash_"+user.id, "csrf_"+user.id, f.now.Add(time.Hour))
		f.users = append(f.users, user.id)
	}
	key := sha256.Sum256([]byte("private-policy-machine-key:" + suffix))
	f.exec(`INSERT INTO paperboat.user_machines(id,user_id,environment_id,platform,architecture,workspace_root,state,seat_state,online,public_identity_key,installation_generation,worker_generation,worker_service_scope,connector_state) VALUES($1,$2,$3,'linux','amd64','/workspace','online','occupied',true,$4,1,1,'system','ready')`, f.machine, f.owner, "env_pp_"+suffix, base64.RawURLEncoding.EncodeToString(key[:]))
	return f
}

func (f *privatePolicyFixture) exec(query string, args ...any) {
	f.t.Helper()
	if _, err := f.store.SQL().ExecContext(f.ctx, query, args...); err != nil {
		f.t.Fatal(err)
	}
}

// preview seeds a ready preview of the given native access mode; its route ID is the lease ID.
func (f *privatePolicyFixture) preview(name, mode, team string) (id, host string) {
	f.t.Helper()
	return f.previewAt(name, mode, team, "foreground", "")
}

// previewAt seeds one with the lease's owner session kind and, when set, its address.
func (f *privatePolicyFixture) previewAt(name, mode, team, sessionKind, address string) (id, host string) {
	f.t.Helper()
	serial := fmt.Sprint(time.Now().UnixNano())
	id, node, epoch := "prv_pp_"+name+"_"+serial, "edge-pp-"+serial, "epoch-pp-"+serial
	seed := "pp_" + name + "_" + serial
	host = "pv-" + strings.ReplaceAll(seed, "_", "-") + ".preview.example.test"
	if address != "" {
		host = address
	}
	f.t.Cleanup(func() {
		_, _ = f.store.SQL().ExecContext(f.ctx, `DELETE FROM paperboat.browser_access_sessions WHERE resource_id=$1`, id)
		_, _ = f.store.SQL().ExecContext(f.ctx, `DELETE FROM paperboat.browser_access_transactions WHERE resource_id=$1`, id)
		_, _ = f.store.SQL().ExecContext(f.ctx, `DELETE FROM paperboat.preview_leases WHERE id=$1`, id)
		_, _ = f.store.SQL().ExecContext(f.ctx, `DELETE FROM paperboat.control_tunnel_nodes WHERE id=$1`, node)
	})
	f.exec(`INSERT INTO paperboat.control_tunnel_nodes(id,edge_pool,protocol_version,process_epoch,endpoint_host,endpoint_tcp_port,endpoint_quic_port,carrier_endpoint_host,carrier_endpoint_tcp_port,carrier_endpoint_quic_port,carrier_server_spki_sha256,carrier_server_certificate_chain_pem,state,ready,capacity,last_heartbeat_at) VALUES($1,'test','1.0',$2,'edge.example.test',24001,24002,'edge.example.test',25001,25002,'sha256:'||repeat('b',64),'test-public-certificate-chain','ready',true,'{}',now())`, node, epoch)
	f.exec(`INSERT INTO paperboat.preview_leases(owner_session_kind,id,endpoint,account_id,actor_id,owner_machine_id,owner_session_id,target_scheme,target_address,access_mode,lease_deadline,allocation_state,edge_state,origin_state,terminal_state,created_at,last_renewed_at,owner_team_id) VALUES($10,$1,$2,$3,$3,$4,$5,'http','127.0.0.1:3000',$6,$7,'ready','ready','ready','active',$8,$8,NULLIF($9,''))`, id, "https://"+host, f.owner, f.machine, "owner-session-"+serial, mode, f.now.Add(time.Hour), f.now, team, sessionKind)
	var publicKey string
	if err := f.store.SQL().QueryRowContext(f.ctx, `SELECT public_identity_key FROM paperboat.user_machines WHERE id=$1`, f.machine).Scan(&publicKey); err != nil {
		f.t.Fatal(err)
	}
	testutil.EnginePreview{Suffix: seed, Lease: id, Account: f.owner, Machine: f.machine, Node: node, Epoch: epoch, PublicKey: publicKey, AccessMode: mode, Now: f.now}.Serve(f.t, f.exec)
	return id, host
}

func (f *privatePolicyFixture) tunnel(name, mode string) (id, route, host string) {
	f.t.Helper()
	return f.tunnelOwned(name, mode, "")
}

// tunnelOwned seeds one that a Team owns when team is set; a tunnel's owner cannot change later.
func (f *privatePolicyFixture) tunnelOwned(name, mode, team string) (id, route, host string) {
	f.t.Helper()
	serial := fmt.Sprint(time.Now().UnixNano())
	id, route = "tun_pp_"+name+"_"+serial, "rte_pp_"+name+"_"+serial
	endpoint := "00000000-0000-4000-8000-" + serial[len(serial)-12:]
	host = endpoint + ".tunnels.example.test"
	f.t.Cleanup(func() {
		_, _ = f.store.SQL().ExecContext(f.ctx, `DELETE FROM paperboat.browser_access_sessions WHERE resource_id=$1`, id)
		_, _ = f.store.SQL().ExecContext(f.ctx, `DELETE FROM paperboat.browser_access_transactions WHERE resource_id=$1`, id)
		_, _ = f.store.SQL().ExecContext(f.ctx, `DELETE FROM paperboat.tunnels WHERE id=$1`, id)
	})
	f.exec(`INSERT INTO paperboat.tunnels(id,account_id,name,desired_state,access_mode,generation,stable_endpoint_id,stable_endpoint,created_by_host_id,created_by_actor_id,summary_code,created_at,updated_at,owner_team_id) VALUES($1,$2,$1,'active',$3,1,$4,$5,$6,$2,'pending',$7,$7,NULLIF($8,''))`, id, f.owner, mode, "endpoint_"+endpoint, "https://"+host, f.machine, f.now, team)
	f.exec(`INSERT INTO paperboat.tunnel_routes(id,tunnel_id,name,protocol,match_type,match_hostname,path_prefix,priority,origin_scheme,origin_address,desired_state,generation,created_by_actor_id,updated_by_actor_id,created_at,updated_at) VALUES($1,$2,'web','http','managed',$3,'/',100,'http','127.0.0.1:3400','active',1,$4,$4,$5,$5)`, route, id, host, f.owner, f.now)
	return id, route, host
}

func (f *privatePolicyFixture) set(kind, id, route, audience string, generation int64, entries ...string) (ViewerPolicy, error) {
	return f.svc.SetViewerPolicy(f.ctx, SetViewerPolicyRequest{ActorAccountID: f.owner, ResourceKind: kind, ResourceID: id, RouteID: route, Audience: audience, AllowedEmails: entries, ExpectedGeneration: generation})
}

func (f *privatePolicyFixture) signIn(user, host string) (string, error) {
	// A new service for each sign-in: the per-visitor limit would otherwise refuse the later ones at random.
	svc, err := NewService(f.store, "https://login.example.test")
	if err != nil {
		return "", err
	}
	svc.now = f.svc.now
	return issueBrowserTokenResult(svc, f.ctx, user, "ses_"+user, host, "/", f.now)
}

func (f *privatePolicyFixture) admits(host string, want map[string]bool) {
	f.t.Helper()
	for user, allowed := range want {
		_, err := f.signIn(user, host)
		if allowed && err != nil {
			f.t.Errorf("%s was refused: %v", user, err)
		}
		if !allowed && !errors.Is(err, ErrAccessDenied) {
			f.t.Errorf("%s = %v, want access denied", user, err)
		}
	}
}

func (f *privatePolicyFixture) policies(id string) (count int) {
	f.t.Helper()
	if err := f.store.SQL().QueryRowContext(f.ctx, `SELECT count(*) FROM paperboat.browser_viewer_policies WHERE resource_id=$1`, id).Scan(&count); err != nil {
		f.t.Fatal(err)
	}
	return count
}

func TestPrivatePreviewAllowListPostgres(t *testing.T) {
	f := newPrivatePolicyFixture(t)
	id, host := f.preview("list", "private", "")
	f.admits(host, map[string]bool{f.owner: true, f.listed: false, f.domain: false})

	if _, err := f.set("preview", id, id, AudiencePublic, 0); !errors.Is(err, ErrPolicyWidening) {
		t.Fatalf("a private preview made public = %v, want ErrPolicyWidening", err)
	}
	if _, err := f.set("preview", id, id, AudienceSpecificEmails, 0, "*"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a bare wildcard = %v, want ErrInvalid", err)
	}
	if f.policies(id) != 0 {
		t.Fatal("a refused policy was saved")
	}
	policy, err := f.set("preview", id, id, AudienceSpecificEmails, 0, "guest@partner.test", "*@company.test")
	if err != nil || !policy.Configured || policy.Generation != 1 || policy.NativeAccessMode != "private" {
		t.Fatalf("private preview allow list = %#v, %v", policy, err)
	}
	f.admits(host, map[string]bool{f.owner: true, f.listed: true, f.domain: true, f.subdomain: false, f.other: false, f.unverified: false})

	token, err := f.signIn(f.domain, host)
	if err != nil {
		t.Fatal(err)
	}
	authorize := func() (Authorization, error) {
		return f.svc.Authorize(f.ctx, AuthorizeRequest{Token: token, Host: host, ResourceKind: "preview", ResourceID: id, RouteID: id})
	}
	grant, err := authorize()
	if err != nil || grant.AccessMode != "private" || grant.AccountID != f.domain || grant.OwnerAccountID != f.owner {
		t.Fatalf("a listed viewer's authorization = %#v, %v", grant, err)
	}
	// The machine's own check of the edge's decision admits the same viewer.
	if _, err = f.svc.AuthorizeGrant(f.ctx, grant.GrantID, host, "preview", id, id); err != nil {
		t.Fatalf("the listed viewer's grant was refused: %v", err)
	}
	// A listed viewer gets browser viewing only, never a machine credential.
	if _, err = f.svc.IssueMachineCredential(f.ctx, MachineCredentialRequest{AccountID: f.domain, Host: host, ResourceKind: "preview", ResourceID: id, RouteID: id, Action: "use", TTL: time.Minute}); !errors.Is(err, ErrNotAuthorized) {
		t.Fatalf("a listed viewer's machine credential = %v, want ErrNotAuthorized", err)
	}
	if _, err = f.set("preview", id, id, AudienceOwner, 1); err != nil {
		t.Fatal(err)
	}
	if _, err = authorize(); !errors.Is(err, ErrNotAuthorized) {
		t.Fatalf("a viewer taken off the list kept the session: %v", err)
	}
	if _, err = f.svc.AuthorizeGrant(f.ctx, grant.GrantID, host, "preview", id, id); !errors.Is(err, ErrNotAuthorized) {
		t.Fatalf("a viewer taken off the list kept the grant: %v", err)
	}
	f.admits(host, map[string]bool{f.owner: true, f.listed: false, f.domain: false})

	// Deleting the preview deletes its policy and the addresses in it.
	f.exec(`DELETE FROM paperboat.browser_access_sessions WHERE resource_id=$1`, id)
	f.exec(`DELETE FROM paperboat.browser_access_transactions WHERE resource_id=$1`, id)
	f.exec(`DELETE FROM paperboat.preview_leases WHERE id=$1`, id)
	if f.policies(id) != 0 {
		t.Fatal("the policy outlived its preview")
	}
}

func TestPublicAndTeamPreviewsTakeNoAllowListPostgres(t *testing.T) {
	f := newPrivatePolicyFixture(t)
	public, _ := f.preview("public", "public", "")
	if _, err := f.set("preview", public, public, AudienceSpecificEmails, 0, "*@company.test"); !errors.Is(err, ErrPolicyEdgeTransition) {
		t.Fatalf("public preview allow list = %v, want ErrPolicyEdgeTransition", err)
	}
	team, _ := f.preview("team", "team", "")
	if _, err := f.set("preview", team, team, AudienceSpecificEmails, 0, "*@company.test"); !errors.Is(err, ErrPolicyWidening) {
		t.Fatalf("team preview allow list = %v, want ErrPolicyWidening", err)
	}
	// A lazy preview's browser path never reads a viewer policy, so none can be saved for it.
	host := "lz-pp-" + f.suffix + ".local.example.test"
	f.exec(`INSERT INTO paperboat.lazy_access_policies(id,hostname,account_id,environment_id,machine_id,installation_generation,generation,target_scheme,target_address,access_mode,ownership_mode,expires_at,authorization_source) VALUES($1,$2,$3,$4,$5,1,1,'http','127.0.0.1:3000','private','persistent_port',now()+interval '1 hour','explicit')`, "lz_pp_"+f.suffix, host, f.owner, "env_pp_"+f.suffix, f.machine)
	t.Cleanup(func() {
		_, _ = f.store.SQL().ExecContext(f.ctx, `DELETE FROM paperboat.lazy_access_policies WHERE id=$1`, "lz_pp_"+f.suffix)
	})
	// As lazy activation publishes it: a lease at the policy's own name.
	lazy, _ := f.previewAt("lazy", "private", "", "lazy_runtime", host)
	if _, err := f.set("preview", lazy, lazy, AudienceSpecificEmails, 0, "*@company.test"); !errors.Is(err, ErrNotAuthorized) {
		t.Fatalf("lazy preview allow list = %v, want ErrNotAuthorized", err)
	}
}

func TestPrivateTunnelAllowListPostgres(t *testing.T) {
	f := newPrivatePolicyFixture(t)
	id, route, host := f.tunnel("private", "private")
	if _, err := f.set("tunnel", id, route, AudienceSpecificEmails, 0, "guest@partner.test", "*@company.test"); err != nil {
		t.Fatalf("private tunnel allow list = %v", err)
	}
	f.admits(host, map[string]bool{f.owner: true, f.listed: true, f.domain: true, f.subdomain: false, f.other: false, f.unverified: false})
	var mode string
	if err := f.store.SQL().QueryRowContext(f.ctx, `SELECT effective_access_mode FROM paperboat.current_tunnel_route_viewer_policy WHERE route_id=$1`, route).Scan(&mode); err != nil || mode != "private" {
		t.Fatalf("edge mode of a listed private tunnel = %q, %v", mode, err)
	}
	if _, err := f.set("tunnel", id, route, AudiencePublic, 1); !errors.Is(err, ErrPolicyWidening) {
		t.Fatalf("a private tunnel made public = %v, want ErrPolicyWidening", err)
	}
	// Deleting the route deletes its policy.
	f.exec(`DELETE FROM paperboat.browser_access_sessions WHERE resource_id=$1`, id)
	f.exec(`DELETE FROM paperboat.browser_access_transactions WHERE resource_id=$1`, id)
	f.exec(`DELETE FROM paperboat.tunnel_routes WHERE id=$1`, route)
	if f.policies(id) != 0 {
		t.Fatal("the policy outlived its route")
	}
}

// A public tunnel lifted to a public audience and later made private keeps a stored "public" audience. It admits the
// owner, who keeps access, and nobody else.
func TestStalePublicPolicyOnPrivateTunnelAdmitsOnlyTheOwnerPostgres(t *testing.T) {
	f := newPrivatePolicyFixture(t)
	id, route, host := f.tunnel("stale", "public")
	if _, err := f.set("tunnel", id, route, AudiencePublic, 0); err != nil {
		t.Fatal(err)
	}
	f.exec(`UPDATE paperboat.tunnels SET access_mode='private' WHERE id=$1`, id)
	f.admits(host, map[string]bool{f.owner: true, f.listed: false, f.domain: false})
}

// A private resource with a list that is later made a team one keeps a stored list that no longer applies. It admits
// the owner and nobody on the list.
func TestStaleAllowListOnTeamTunnelAdmitsOnlyTheOwnerPostgres(t *testing.T) {
	f := newPrivatePolicyFixture(t)
	id, route, host := f.tunnel("toteam", "private")
	if _, err := f.set("tunnel", id, route, AudienceSpecificEmails, 0, "guest@partner.test", "*@company.test"); err != nil {
		t.Fatal(err)
	}
	f.exec(`UPDATE paperboat.tunnels SET access_mode='team' WHERE id=$1`, id)
	f.admits(host, map[string]bool{f.owner: true, f.listed: false, f.domain: false})
}

// On a Team's private preview the list narrows the Team's own rules; it never adds someone they would refuse.
func TestTeamPrivatePreviewAllowListOnlyNarrowsPostgres(t *testing.T) {
	f := newPrivatePolicyFixture(t)
	team := "team-pp-" + f.suffix
	t.Cleanup(func() {
		_, _ = f.store.SQL().ExecContext(f.ctx, `DELETE FROM paperboat.teams WHERE team_id=$1`, team)
	})
	f.exec(`INSERT INTO paperboat.teams(team_id,owner_account,generation) VALUES($1,$2,1)`, team, f.owner)
	for _, member := range []string{f.owner, f.domain, f.other} {
		f.exec(`INSERT INTO paperboat.team_members(team_id,account_id,membership_generation,role,active) VALUES($1,$2,1,'member',true)`, team, member)
	}
	f.exec(`INSERT INTO paperboat.billing_workspaces(id,team_id,state,product_code,quantity,period_start,period_end) VALUES($1,$2,'active','team-pro',3,$3,$4)`, "ws_pp_"+f.suffix, team, f.now.Add(-time.Hour), f.now.Add(time.Hour))
	id, host := f.preview("owned", "private", team)
	f.exec(`INSERT INTO paperboat.team_resource_bindings(team_id,resource_kind,resource_id,owner_account,active) VALUES($1,'preview',$2,$3,true) ON CONFLICT DO NOTHING`, team, id, f.owner)
	for _, member := range []string{f.domain, f.other} {
		f.exec(`INSERT INTO paperboat.team_resource_grants(team_id,account_id,resource_kind,resource_id,permission,generation,active) VALUES($1,$2,'preview',$3,'use',1,true)`, team, member, id)
	}
	f.admits(host, map[string]bool{f.domain: true, f.other: true, f.listed: false})
	credential := func(account string) (MachineCredentialResult, error) {
		return f.svc.IssueMachineCredential(f.ctx, MachineCredentialRequest{AccountID: account, Host: host, ResourceKind: "preview", ResourceID: id, RouteID: id, Action: "use", TTL: time.Minute})
	}
	before, err := credential(f.other)
	if err != nil {
		t.Fatalf("a member's machine credential without a policy = %v", err)
	}

	// guest@partner.test is listed but is not in the Team; other@partner.test is in the Team but not listed.
	if _, err = f.set("preview", id, id, AudienceSpecificEmails, 0, "guest@partner.test", "*@company.test"); err != nil {
		t.Fatal(err)
	}
	f.admits(host, map[string]bool{f.owner: true, f.domain: true, f.other: false, f.listed: false})
	if _, err = credential(f.other); !errors.Is(err, ErrNotAuthorized) {
		t.Fatalf("a member left off the list got a machine credential: %v", err)
	}
	if _, err = f.svc.AuthorizeMachineCredential(f.ctx, before.Token, host, "preview", id, id, "use"); !errors.Is(err, ErrNotAuthorized) {
		t.Fatalf("a machine credential issued before the list outlived it: %v", err)
	}
	if _, err = credential(f.domain); err != nil {
		t.Fatalf("a listed member's machine credential = %v", err)
	}
}

// listThenPublic saves an allow list on a public tunnel and then makes it public again.
func (f *privatePolicyFixture) listThenPublic(id, route, host string) {
	f.t.Helper()
	if _, err := f.set("tunnel", id, route, AudienceSpecificEmails, 0, "guest@partner.test", "*@company.test"); err != nil {
		f.t.Fatalf("allow list on a public tunnel = %v", err)
	}
	if mode := f.edgeMode(route); mode != "private" {
		f.t.Fatalf("edge mode of a listed public tunnel = %q", mode)
	}
	f.admits(host, map[string]bool{f.owner: true, f.listed: true, f.domain: true, f.subdomain: false, f.other: false, f.unverified: false})
	if _, err := f.set("tunnel", id, route, AudiencePublic, 1); err != nil {
		f.t.Fatalf("clearing the list back to public = %v", err)
	}
	if mode := f.edgeMode(route); mode != "public" {
		f.t.Fatalf("edge mode after clearing back to public = %q", mode)
	}
	f.takesNoSignIn(host, ErrNotAuthorized)
}

func TestPaidPublicTunnelTakesAnAllowListAndClearsBackToPublicPostgres(t *testing.T) {
	f := newPrivatePolicyFixture(t)
	t.Cleanup(func() {
		_, _ = f.store.SQL().ExecContext(f.ctx, `DELETE FROM paperboat.billing_workspaces WHERE id=$1`, "personal:"+f.owner)
	})
	f.exec(`INSERT INTO paperboat.billing_workspaces(id,user_id,provider_customer_id,provider_subscription_id,product_code,state,quantity,period_start,period_end) VALUES($1,$2,$3,$4,'pro-month','active',1,$5,$6)`, "personal:"+f.owner, f.owner, "cus_pb_"+f.suffix, "sub_pb_"+f.suffix, f.now.Add(-time.Hour), f.now.Add(time.Hour))
	team := f.paidTeam("listpublic")
	for _, plan := range []struct{ name, team string }{{"pro", ""}, {"team", team}} {
		t.Run(plan.name, func(t *testing.T) {
			g := *f
			g.t = t
			id, route, host := g.tunnelOwned("lp"+plan.name, "public", plan.team)
			g.listThenPublic(id, route, host)
		})
	}
}

// Allow lists are not a paid feature: an account with no plan sets them too.
func TestFreeAccountSetsAllowListsPostgres(t *testing.T) {
	f := newPrivatePolicyFixture(t)
	var plans int
	if err := f.store.SQL().QueryRowContext(f.ctx, `SELECT count(*) FROM paperboat.billing_workspaces WHERE user_id=$1`, f.owner).Scan(&plans); err != nil || plans != 0 {
		t.Fatalf("the free account has %d plans, %v", plans, err)
	}
	preview, previewHost := f.preview("free", "private", "")
	if _, err := f.set("preview", preview, preview, AudienceSpecificEmails, 0, "guest@partner.test", "*@company.test"); err != nil {
		t.Fatalf("allow list on a free private preview = %v", err)
	}
	private, privateRoute, privateHost := f.tunnel("freeprivate", "private")
	if _, err := f.set("tunnel", private, privateRoute, AudienceSpecificEmails, 0, "guest@partner.test", "*@company.test"); err != nil {
		t.Fatalf("allow list on a free private tunnel = %v", err)
	}
	for _, host := range []string{previewHost, privateHost} {
		f.admits(host, map[string]bool{f.owner: true, f.listed: true, f.domain: true, f.other: false})
	}
	public, publicRoute, publicHost := f.tunnel("freepublic", "public")
	f.listThenPublic(public, publicRoute, publicHost)
}
