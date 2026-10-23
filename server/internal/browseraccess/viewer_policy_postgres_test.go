package browseraccess

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/auth"
	"github.com/pinksaucepasta/paperboat-server/internal/config"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
)

func TestBrowserViewerPolicyPostgres(t *testing.T) {
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
	defer store.Close()
	if os.Getenv("PAPERBOAT_TEST_SCHEMA_READY") != "1" {
		if err = db.Migrate(ctx, store); err != nil {
			t.Fatal(err)
		}
	}

	suffix := fmt.Sprint(time.Now().UnixNano())
	owner := "usr_vp_owner_" + suffix
	teamViewer := "usr_vp_team_" + suffix
	emailViewer := "usr_vp_email_" + suffix
	domainViewer := "usr_vp_domain_" + suffix
	subdomainViewer := "usr_vp_subdomain_" + suffix
	suffixViewer := "usr_vp_suffix_" + suffix
	unverifiedViewer := "usr_vp_unverified_" + suffix
	team := "team_vp_" + suffix
	tunnel := "tun_vp_" + suffix
	routeA := "rte_vp_a_" + suffix
	routeB := "rte_vp_b_" + suffix
	machine := "host_vp_" + suffix
	endpointID := "00000000-0000-4000-8000-" + suffix[len(suffix)-12:]
	host := endpointID + ".tunnels.example.test"
	now := time.Now().UTC().Truncate(time.Microsecond)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, e := store.SQL().ExecContext(ctx, q, args...); e != nil {
			t.Fatal(e)
		}
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = store.SQL().ExecContext(cleanupCtx, `DELETE FROM paperboat.browser_access_sessions WHERE resource_id=$1`, tunnel)
		_, _ = store.SQL().ExecContext(cleanupCtx, `DELETE FROM paperboat.browser_access_transactions WHERE resource_id=$1`, tunnel)
		_, _ = store.SQL().ExecContext(cleanupCtx, `DELETE FROM paperboat.tunnels WHERE id=$1`, tunnel)
		_, _ = store.SQL().ExecContext(cleanupCtx, `DELETE FROM paperboat.user_machines WHERE id=$1`, machine)
		_, _ = store.SQL().ExecContext(cleanupCtx, `DELETE FROM paperboat.teams WHERE team_id=$1`, team)
		_, _ = store.SQL().ExecContext(cleanupCtx, `DELETE FROM paperboat.users WHERE id IN($1,$2,$3,$4,$5,$6,$7)`, owner, teamViewer, emailViewer, domainViewer, subdomainViewer, suffixViewer, unverifiedViewer)
	})

	exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,email_verified,status) VALUES
($1,$1,'owner@test.invalid',false,'active'),
($2,$2,'team@elsewhere.test',true,'active'),
($3,$3,'person@company.test',true,'active'),
($4,$4,'person@company.test',true,'active'),
($5,$5,'person@sub.company.test',true,'active'),
($6,$6,'person@notcompany.test',true,'active'),
($7,$7,'person@company.test',false,'active')`, owner, teamViewer, emailViewer, domainViewer, subdomainViewer, suffixViewer, unverifiedViewer)
	exec(`INSERT INTO paperboat.user_machines(id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state) VALUES($1,$2,$3,'browser-host','linux','amd64','/workspace','offline','released')`, machine, owner, "env_vp_"+suffix)
	exec(`INSERT INTO paperboat.tunnels(id,account_id,name,desired_state,access_mode,generation,stable_endpoint_id,stable_endpoint,created_by_host_id,created_by_actor_id,created_at,updated_at) VALUES($1,$2,'viewer-policy','active','public',1,$3,$4,$5,$2,$6,$6)`, tunnel, owner, "endpoint_"+endpointID, "https://"+host, machine, now)
	exec(`INSERT INTO paperboat.tunnel_routes(id,tunnel_id,name,protocol,match_type,match_hostname,path_prefix,priority,origin_scheme,origin_address,desired_state,generation,created_by_actor_id,updated_by_actor_id,created_at,updated_at) VALUES
($1,$2,'a','http','managed',$3,'/a',100,'http','127.0.0.1:3100','active',1,$4,$4,$5,$5),
($6,$2,'b','http','managed',$3,'/b',100,'http','127.0.0.1:3200','active',1,$4,$4,$5,$5)`, routeA, tunnel, host, owner, now, routeB)
	exec(`INSERT INTO paperboat.teams(team_id,owner_account,generation) VALUES($1,$2,1)`, team, owner)
	// The resource owner is deliberately not a team member; owner access remains
	// independent from the selected team's browser audience.
	exec(`INSERT INTO paperboat.team_members(team_id,account_id,membership_generation,role,active) VALUES($1,$2,1,'member',true)`, team, teamViewer)
	exec(`INSERT INTO paperboat.team_resource_bindings(team_id,resource_kind,resource_id,owner_account,active) VALUES($1,'tunnel',$2,$3,true)`, team, tunnel, owner)
	sessionIDs := map[string]string{}
	for _, user := range []string{owner, teamViewer, emailViewer, domainViewer, subdomainViewer, suffixViewer, unverifiedViewer} {
		session := "ses_vp_" + user
		sessionIDs[user] = session
		exec(`INSERT INTO paperboat.sessions(id,user_id,session_hash,csrf_hash,expires_at) VALUES($1,$2,$3,$4,$5)`, session, user, "hash_"+user, "csrf_"+user, now.Add(time.Hour))
	}

	svc, err := NewService(store, "https://login.example.test")
	if err != nil {
		t.Fatal(err)
	}
	svc.now = func() time.Time { return now }
	set := func(audience string, generation int64, teamID, domain string, emails ...string) (ViewerPolicy, error) {
		return svc.SetViewerPolicy(ctx, SetViewerPolicyRequest{
			ActorAccountID: owner, ResourceKind: "tunnel", ResourceID: tunnel, RouteID: routeA,
			Audience: audience, TeamID: teamID, EmailDomain: domain, AllowedEmails: emails,
			ExpectedGeneration: generation,
		})
	}

	// The parent route lock serializes first writes, so only one generation-zero
	// writer can create the policy.
	var wg sync.WaitGroup
	results := make(chan error, 2)
	wg.Add(2)
	for _, audience := range []string{AudienceOwner, AudienceSpecificEmails} {
		go func(audience string) {
			defer wg.Done()
			var e error
			if audience == AudienceOwner {
				_, e = set(audience, 0, "", "")
			} else {
				_, e = set(audience, 0, "", "", "person@company.test")
			}
			results <- e
		}(audience)
	}
	wg.Wait()
	close(results)
	wins, conflicts := 0, 0
	for result := range results {
		if result == nil {
			wins++
		} else if errors.Is(result, ErrPolicyConflict) {
			conflicts++
		} else {
			t.Fatalf("generation-zero policy writer error: %v", result)
		}
	}
	if wins != 1 || conflicts != 1 {
		t.Fatalf("generation-zero writers: winners=%d conflicts=%d", wins, conflicts)
	}
	first, err := svc.GetViewerPolicy(ctx, owner, "tunnel", tunnel, routeA)
	if err != nil || !first.Configured || first.Generation != 1 {
		t.Fatalf("initial policy = %#v, %v", first, err)
	}

	policy, err := set(AudienceTeam, 1, team, "")
	if err != nil || policy.Audience != AudienceTeam || policy.Generation != 2 {
		t.Fatalf("team policy = %#v, %v", policy, err)
	}
	if _, err = svc.SetViewerPolicy(ctx, SetViewerPolicyRequest{ActorAccountID: owner, ResourceKind: "tunnel", ResourceID: tunnel, RouteID: routeB, Audience: AudienceOwner, ExpectedGeneration: 0}); err != nil {
		t.Fatalf("unrelated route owner policy = %v", err)
	}
	ownerRouteBToken := issueBrowserToken(t, svc, ctx, owner, sessionIDs[owner], host, "/b/page", now)
	teamAuth, err := func() (Authorization, error) {
		token := issueBrowserToken(t, svc, ctx, teamViewer, sessionIDs[teamViewer], host, "/a/page", now)
		return svc.Authorize(ctx, AuthorizeRequest{Token: token, Host: host, ResourceKind: "tunnel", ResourceID: tunnel, RouteID: routeA})
	}()
	if err != nil || teamAuth.AccessMode != "private" || teamAuth.TeamID != "" || teamAuth.MembershipGeneration != 0 {
		t.Fatalf("browser team audience claimed native team authority: %#v, %v", teamAuth, err)
	}
	var policyTeam string
	var policyTeamGeneration, policyMembershipGeneration, policyBindingGeneration int64
	if err = store.SQL().QueryRowContext(ctx, `SELECT viewer_policy_team_id,viewer_policy_team_generation,viewer_policy_membership_generation,viewer_policy_binding_generation FROM paperboat.browser_access_sessions WHERE account_id=$1 AND route_id=$2 AND revoked_at IS NULL ORDER BY issued_at DESC LIMIT 1`, teamViewer, routeA).Scan(&policyTeam, &policyTeamGeneration, &policyMembershipGeneration, &policyBindingGeneration); err != nil {
		t.Fatal(err)
	}
	if policyTeam != team || policyTeamGeneration != 1 || policyMembershipGeneration != 1 || policyBindingGeneration != 1 {
		t.Fatalf("stored viewer-policy team authority = %s/%d/%d/%d", policyTeam, policyTeamGeneration, policyMembershipGeneration, policyBindingGeneration)
	}
	ownerToken := issueBrowserToken(t, svc, ctx, owner, sessionIDs[owner], host, "/a/page", now)
	ownerAuth, err := svc.Authorize(ctx, AuthorizeRequest{Token: ownerToken, Host: host, ResourceKind: "tunnel", ResourceID: tunnel, RouteID: routeA})
	if err != nil || ownerAuth.AccessMode != "private" || ownerAuth.TeamID != "" || ownerAuth.MembershipGeneration != 0 {
		t.Fatalf("resource owner without team membership denied or claimed team authority: %#v, %v", ownerAuth, err)
	}
	if _, err = svc.IssueMachineCredential(ctx, MachineCredentialRequest{AccountID: teamViewer, Host: host, ResourceKind: "tunnel", ResourceID: tunnel, RouteID: routeA, Action: "use", TTL: time.Minute}); !errors.Is(err, ErrNotAuthorized) {
		t.Fatalf("browser-only team audience gained native machine grant: %v", err)
	}
	teamToken := issueBrowserToken(t, svc, ctx, teamViewer, sessionIDs[teamViewer], host, "/a/page?next=%2Fok", now)
	if _, err = svc.Authorize(ctx, AuthorizeRequest{Token: teamToken, Host: host, ResourceKind: "tunnel", ResourceID: tunnel, RouteID: routeA}); err != nil {
		t.Fatalf("current team member denied without native grant: %v", err)
	}
	if _, err = svc.Authorize(ctx, AuthorizeRequest{Token: teamToken, Host: host, ResourceKind: "tunnel", ResourceID: tunnel, RouteID: routeB}); !errors.Is(err, ErrNotAuthorized) {
		t.Fatalf("route A session authorized route B: %v", err)
	}
	exec(`UPDATE paperboat.team_members SET active=false WHERE team_id=$1 AND account_id=$2`, team, teamViewer)
	if _, err = svc.Authorize(ctx, AuthorizeRequest{Token: teamToken, Host: host, ResourceKind: "tunnel", ResourceID: tunnel, RouteID: routeA}); !errors.Is(err, ErrNotAuthorized) {
		t.Fatalf("removed team member kept browser access: %v", err)
	}
	exec(`UPDATE paperboat.team_members SET active=true,membership_generation=membership_generation+1 WHERE team_id=$1 AND account_id=$2`, team, teamViewer)
	if _, err = svc.Authorize(ctx, AuthorizeRequest{Token: teamToken, Host: host, ResourceKind: "tunnel", ResourceID: tunnel, RouteID: routeA}); !errors.Is(err, ErrNotAuthorized) {
		t.Fatalf("removed-and-readded team member revived an old browser session: %v", err)
	}
	rejoinedTeamToken := issueBrowserToken(t, svc, ctx, teamViewer, sessionIDs[teamViewer], host, "/a/page", now)
	if _, err = svc.Authorize(ctx, AuthorizeRequest{Token: rejoinedTeamToken, Host: host, ResourceKind: "tunnel", ResourceID: tunnel, RouteID: routeA}); err != nil {
		t.Fatalf("readded team member could not establish a fresh browser session: %v", err)
	}
	exec(`UPDATE paperboat.team_resource_bindings SET active=false,generation=generation+1 WHERE team_id=$1 AND resource_kind='tunnel' AND resource_id=$2`, team, tunnel)
	if _, err = svc.Authorize(ctx, AuthorizeRequest{Token: rejoinedTeamToken, Host: host, ResourceKind: "tunnel", ResourceID: tunnel, RouteID: routeA}); !errors.Is(err, ErrNotAuthorized) {
		t.Fatalf("unbound team retained browser access: %v", err)
	}
	exec(`UPDATE paperboat.team_resource_bindings SET active=true,generation=generation+1 WHERE team_id=$1 AND resource_kind='tunnel' AND resource_id=$2`, team, tunnel)
	if _, err = svc.Authorize(ctx, AuthorizeRequest{Token: rejoinedTeamToken, Host: host, ResourceKind: "tunnel", ResourceID: tunnel, RouteID: routeA}); !errors.Is(err, ErrNotAuthorized) {
		t.Fatalf("unbound-and-rebound team revived an old browser session: %v", err)
	}
	reboundTeamToken := issueBrowserToken(t, svc, ctx, teamViewer, sessionIDs[teamViewer], host, "/a/page", now)
	if _, err = svc.Authorize(ctx, AuthorizeRequest{Token: reboundTeamToken, Host: host, ResourceKind: "tunnel", ResourceID: tunnel, RouteID: routeA}); err != nil {
		t.Fatalf("rebound team member could not establish a fresh browser session: %v", err)
	}
	exec(`UPDATE paperboat.teams SET suspended=true,suspended_at=$2,suspended_by=$3,generation=generation+1 WHERE team_id=$1`, team, now, owner)
	if _, err = svc.Authorize(ctx, AuthorizeRequest{Token: reboundTeamToken, Host: host, ResourceKind: "tunnel", ResourceID: tunnel, RouteID: routeA}); !errors.Is(err, ErrNotAuthorized) {
		t.Fatalf("suspended team retained browser access: %v", err)
	}
	exec(`UPDATE paperboat.teams SET suspended=false,suspended_at=NULL,suspended_by=NULL,generation=generation+1 WHERE team_id=$1`, team)
	if _, err = svc.Authorize(ctx, AuthorizeRequest{Token: reboundTeamToken, Host: host, ResourceKind: "tunnel", ResourceID: tunnel, RouteID: routeA}); !errors.Is(err, ErrNotAuthorized) {
		t.Fatalf("suspended-and-reactivated team revived an old browser session: %v", err)
	}
	reactivatedTeamToken := issueBrowserToken(t, svc, ctx, teamViewer, sessionIDs[teamViewer], host, "/a/page", now)
	if _, err = svc.Authorize(ctx, AuthorizeRequest{Token: reactivatedTeamToken, Host: host, ResourceKind: "tunnel", ResourceID: tunnel, RouteID: routeA}); err != nil {
		t.Fatalf("reactivated team member could not establish a fresh browser session: %v", err)
	}
	if _, err = svc.Authorize(ctx, AuthorizeRequest{Token: ownerRouteBToken, Host: host, ResourceKind: "tunnel", ResourceID: tunnel, RouteID: routeB}); err != nil {
		t.Fatalf("team changes revoked an unrelated route's owner session: %v", err)
	}

	policy, err = set(AudienceSpecificEmails, 2, "", "", "Person@Company.test")
	if err != nil || policy.Generation != 3 || len(policy.AllowedEmails) != 1 || policy.AllowedEmails[0] != "person@company.test" {
		t.Fatalf("exact-email policy = %#v, %v", policy, err)
	}
	oldTeamToken := teamToken
	emailToken := issueBrowserToken(t, svc, ctx, emailViewer, sessionIDs[emailViewer], host, "/a/page", now)
	if _, err = svc.Authorize(ctx, AuthorizeRequest{Token: emailToken, Host: host, ResourceKind: "tunnel", ResourceID: tunnel, RouteID: routeA}); err != nil {
		t.Fatalf("verified exact email denied: %v", err)
	}
	if _, err = issueBrowserTokenResult(svc, ctx, teamViewer, sessionIDs[teamViewer], host, "/a/page", now); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("team member retained access after audience change: %v", err)
	}
	if _, err = svc.IssueMachineCredential(ctx, MachineCredentialRequest{AccountID: emailViewer, Host: host, ResourceKind: "tunnel", ResourceID: tunnel, RouteID: routeA, Action: "use", TTL: time.Minute}); !errors.Is(err, ErrNotAuthorized) {
		t.Fatalf("email viewer gained native machine grant: %v", err)
	}
	if _, err = svc.Authorize(ctx, AuthorizeRequest{Token: oldTeamToken, Host: host, ResourceKind: "tunnel", ResourceID: tunnel, RouteID: routeA}); !errors.Is(err, ErrNotAuthorized) {
		t.Fatalf("policy change did not revoke previous browser session: %v", err)
	}
	ownerToken = issueBrowserToken(t, svc, ctx, owner, sessionIDs[owner], host, "/a/page", now)
	if _, err = svc.Authorize(ctx, AuthorizeRequest{Token: ownerToken, Host: host, ResourceKind: "tunnel", ResourceID: tunnel, RouteID: routeA}); err != nil {
		t.Fatalf("resource owner denied by restricted policy: %v", err)
	}

	policy, err = set(AudienceEmailDomain, 3, "", "company.test")
	if err != nil || policy.Generation != 4 {
		t.Fatalf("domain policy = %#v, %v", policy, err)
	}
	domainToken := issueBrowserToken(t, svc, ctx, domainViewer, sessionIDs[domainViewer], host, "/a/page", now)
	if _, err = svc.Authorize(ctx, AuthorizeRequest{Token: domainToken, Host: host, ResourceKind: "tunnel", ResourceID: tunnel, RouteID: routeA}); err != nil {
		t.Fatalf("verified exact-domain email denied: %v", err)
	}
	for _, user := range []string{subdomainViewer, suffixViewer, unverifiedViewer} {
		if _, err = issueBrowserTokenResult(svc, ctx, user, sessionIDs[user], host, "/a/page", now); !errors.Is(err, ErrAccessDenied) {
			t.Errorf("domain audience incorrectly authorized %s: %v", user, err)
		}
	}
	if _, err = svc.Authorize(ctx, AuthorizeRequest{Token: emailToken, Host: host, ResourceKind: "tunnel", ResourceID: tunnel, RouteID: routeA}); !errors.Is(err, ErrNotAuthorized) {
		t.Fatalf("domain change did not revoke exact-email session: %v", err)
	}

	publicPolicy, err := set(AudiencePublic, 4, "", "")
	if err != nil || publicPolicy.Audience != AudiencePublic || publicPolicy.Generation != 5 {
		t.Fatalf("restricted-to-public transition = %#v, %v", publicPolicy, err)
	}
	if _, err = issueBrowserTokenResult(svc, ctx, owner, sessionIDs[owner], host, "/a/page", now); !errors.Is(err, ErrNotAuthorized) {
		t.Fatalf("public audience issued an unnecessary browser session: %v", err)
	}
	publicTunnel := "tun_vp_public_" + suffix
	publicRoute := "rte_vp_public_" + suffix
	publicEndpointID := "10000000-0000-4000-8000-" + suffix[len(suffix)-12:]
	publicHost := publicEndpointID + ".tunnels.example.test"
	exec(`INSERT INTO paperboat.tunnels(id,account_id,name,desired_state,access_mode,generation,stable_endpoint_id,stable_endpoint,created_by_host_id,created_by_actor_id,created_at,updated_at) VALUES($1,$2,'viewer-policy-public','active','public',1,$3,$4,$5,$2,$6,$6)`, publicTunnel, owner, "endpoint_"+publicEndpointID, "https://"+publicHost, machine, now)
	exec(`INSERT INTO paperboat.tunnel_routes(id,tunnel_id,name,protocol,match_type,match_hostname,path_prefix,priority,origin_scheme,origin_address,desired_state,generation,created_by_actor_id,updated_by_actor_id,created_at,updated_at) VALUES($1,$2,'public','http','managed',$3,'/',100,'http','127.0.0.1:3300','active',1,$4,$4,$5,$5)`, publicRoute, publicTunnel, publicHost, owner, now)
	t.Cleanup(func() {
		_, _ = store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.tunnels WHERE id=$1`, publicTunnel)
	})
	policy, err = svc.SetViewerPolicy(ctx, SetViewerPolicyRequest{ActorAccountID: owner, ResourceKind: "tunnel", ResourceID: publicTunnel, RouteID: publicRoute, Audience: AudienceOwner, ExpectedGeneration: 0})
	if err != nil || !policy.Configured || policy.Generation != 1 || policy.ServingState != "pending" {
		t.Fatalf("public-to-restricted policy was not saved as pending: %#v, %v", policy, err)
	}
	privateTunnel := "tun_vp_native_private_" + suffix
	privateRoute := "rte_vp_native_private_" + suffix
	privateEndpointID := "20000000-0000-4000-8000-" + suffix[len(suffix)-12:]
	privateHost := privateEndpointID + ".tunnels.example.test"
	exec(`INSERT INTO paperboat.tunnels(id,account_id,name,desired_state,access_mode,generation,stable_endpoint_id,stable_endpoint,created_by_host_id,created_by_actor_id,summary_code,created_at,updated_at) VALUES($1,$2,'viewer-policy-native-private','active','private',1,$3,$4,$5,$2,'pending',$6,$6)`, privateTunnel, owner, "endpoint_"+privateEndpointID, "https://"+privateHost, machine, now)
	exec(`INSERT INTO paperboat.tunnel_routes(id,tunnel_id,name,protocol,match_type,match_hostname,path_prefix,priority,origin_scheme,origin_address,desired_state,generation,created_by_actor_id,updated_by_actor_id,created_at,updated_at) VALUES($1,$2,'private','http','managed',$3,'/',100,'http','127.0.0.1:3400','active',1,$4,$4,$5,$5)`, privateRoute, privateTunnel, privateHost, owner, now)
	t.Cleanup(func() {
		_, _ = store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.tunnels WHERE id=$1`, privateTunnel)
	})
	if _, err = svc.SetViewerPolicy(ctx, SetViewerPolicyRequest{ActorAccountID: owner, ResourceKind: "tunnel", ResourceID: privateTunnel, RouteID: privateRoute, Audience: AudienceSpecificEmails, AllowedEmails: []string{"person@company.test"}, ExpectedGeneration: 0}); !errors.Is(err, ErrPolicyWidening) {
		t.Fatalf("native-private specific-email policy = %v, want ErrPolicyWidening", err)
	}
	nativeTeamTunnel := "tun_vp_native_team_" + suffix
	nativeTeamRoute := "rte_vp_native_team_" + suffix
	nativeTeamEndpointID := "30000000-0000-4000-8000-" + suffix[len(suffix)-12:]
	nativeTeamHost := nativeTeamEndpointID + ".tunnels.example.test"
	exec(`INSERT INTO paperboat.tunnels(id,account_id,name,desired_state,access_mode,generation,stable_endpoint_id,stable_endpoint,created_by_host_id,created_by_actor_id,summary_code,created_at,updated_at) VALUES($1,$2,'viewer-policy-native-team','active','team',1,$3,$4,$5,$2,'pending',$6,$6)`, nativeTeamTunnel, owner, "endpoint_"+nativeTeamEndpointID, "https://"+nativeTeamHost, machine, now)
	exec(`INSERT INTO paperboat.tunnel_routes(id,tunnel_id,name,protocol,match_type,match_hostname,path_prefix,priority,origin_scheme,origin_address,desired_state,generation,created_by_actor_id,updated_by_actor_id,created_at,updated_at) VALUES($1,$2,'team','http','managed',$3,'/',100,'http','127.0.0.1:3500','active',1,$4,$4,$5,$5)`, nativeTeamRoute, nativeTeamTunnel, nativeTeamHost, owner, now)
	t.Cleanup(func() {
		_, _ = store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.tunnels WHERE id=$1`, nativeTeamTunnel)
	})
	if _, err = svc.SetViewerPolicy(ctx, SetViewerPolicyRequest{ActorAccountID: owner, ResourceKind: "tunnel", ResourceID: nativeTeamTunnel, RouteID: nativeTeamRoute, Audience: AudienceOwner, ExpectedGeneration: 0}); err != nil {
		t.Fatalf("native-team owner policy = %v", err)
	}
	var effectiveMode string
	if err = store.SQL().QueryRowContext(ctx, `SELECT effective_access_mode FROM paperboat.current_tunnel_route_viewer_policy WHERE route_id=$1`, nativeTeamRoute).Scan(&effectiveMode); err != nil || effectiveMode != "private" {
		t.Fatalf("native-team owner edge mode = %q, %v; want private", effectiveMode, err)
	}
	nativeTeamOwnerToken := issueBrowserToken(t, svc, ctx, owner, sessionIDs[owner], nativeTeamHost, "/", now)
	nativeTeamOwnerAuth, err := svc.Authorize(ctx, AuthorizeRequest{Token: nativeTeamOwnerToken, Host: nativeTeamHost, ResourceKind: "tunnel", ResourceID: nativeTeamTunnel, RouteID: nativeTeamRoute})
	if err != nil || nativeTeamOwnerAuth.AccessMode != "private" || nativeTeamOwnerAuth.TeamID != "" {
		t.Fatalf("native-team route owner browser authorization = %#v, %v", nativeTeamOwnerAuth, err)
	}
}

func issueBrowserToken(t *testing.T, svc *Service, ctx context.Context, user, session, host, path string, now time.Time) string {
	t.Helper()
	token, err := issueBrowserTokenResult(svc, ctx, user, session, host, path, now)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func issueBrowserTokenResult(svc *Service, ctx context.Context, user, session, host, path string, now time.Time) (string, error) {
	begin, err := svc.Begin(ctx, BeginRequest{Host: host, ReturnPath: path})
	if err != nil {
		return "", err
	}
	issued, err := svc.Issue(ctx, IssueRequest{TransactionID: begin.TransactionID, Principal: auth.Session{ID: session, UserID: user, ExpiresAt: now.Add(time.Hour)}})
	if err != nil {
		return "", err
	}
	redeemed, err := svc.Redeem(ctx, RedeemRequest{TransactionID: begin.TransactionID, State: begin.State, Handoff: issued.Handoff, Host: host})
	if err != nil {
		return "", err
	}
	return redeemed.Token, nil
}
