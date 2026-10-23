package browseraccess

import (
	"context"
	"errors"
	"fmt"
	"github.com/google/uuid"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/auth"
	"github.com/pinksaucepasta/paperboat-server/internal/config"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
)

func TestBrowserAccessPostgres(t *testing.T) {
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
	owner := "usr_ba_owner_" + suffix
	viewer := "usr_ba_viewer_" + suffix
	session := "ses_ba_" + suffix
	tunnel := "tun_ba_" + suffix
	route := "rte_ba_" + suffix
	hostMachine := "host_ba_" + suffix
	team := "team_ba_" + suffix
	secondTeam := "team_ba_second_" + suffix
	host := uuid.NewString() + ".tunnels.example.test"
	now := time.Now().UTC().Truncate(time.Microsecond)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, e := store.SQL().ExecContext(ctx, q, args...); e != nil {
			t.Fatal(e)
		}
	}
	t.Cleanup(func() {
		_, _ = store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.user_machines WHERE id=$1`, hostMachine)
		_, _ = store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.teams WHERE team_id IN($1,$2)`, team, secondTeam)
		_, _ = store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.users WHERE id IN($1,$2)`, owner, viewer)
	})
	exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@test.invalid','active'),($2,$2,$2||'@test.invalid','active')`, owner, viewer)
	exec(`INSERT INTO paperboat.user_machines(id,user_id,environment_id,alias,description,platform,architecture,workspace_root,state,seat_state) VALUES($1,$2,$3,'browser-host','Browser host','linux','amd64','/workspace','offline','released')`, hostMachine, owner, "env_"+suffix)
	exec(`INSERT INTO paperboat.sessions(id,user_id,session_hash,csrf_hash,expires_at) VALUES($1,$2,$3,$4,$5)`, session, owner, "hash_"+suffix, "csrf_"+suffix, now.Add(2*time.Hour))
	exec(`INSERT INTO paperboat.tunnels(id,account_id,name,desired_state,access_mode,generation,stable_endpoint_id,stable_endpoint,created_by_host_id,created_by_actor_id,created_at,updated_at) VALUES($1,$2,$1,'active','private',3,$3,$4,$5,$2,$6,$6)`, tunnel, owner, "endpoint_"+strings.TrimSuffix(host, ".tunnels.example.test"), "https://"+host, hostMachine, now)
	exec(`INSERT INTO paperboat.tunnel_routes(id,tunnel_id,name,protocol,match_type,origin_scheme,origin_address,generation,desired_state,created_by_actor_id,updated_by_actor_id,created_at,updated_at) VALUES($1,$2,'web','http','catch_all','http','127.0.0.1:3000',7,'active',$3,$3,$4,$4)`, route, tunnel, owner, now)
	svc, err := NewService(store, "https://login.example.test")
	if err != nil {
		t.Fatal(err)
	}
	svc.now = func() time.Time { return now }
	begin, err := svc.Begin(ctx, BeginRequest{Host: host, ReturnPath: "/ok?q=1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = svc.Issue(ctx, IssueRequest{TransactionID: begin.TransactionID, Principal: auth.Session{ID: session, UserID: owner, ExpiresAt: now.Add(2 * time.Hour)}}); err != nil {
		t.Fatal(err)
	}
	var issue IssueResult // a second issue must fail and cannot mint another handoff.
	if issue, err = svc.Issue(ctx, IssueRequest{TransactionID: begin.TransactionID, Principal: auth.Session{ID: session, UserID: owner}}); err == nil || issue.Handoff != "" {
		t.Fatal("duplicate issue succeeded")
	}
	// A signed-in account without access is refused as such, not as an unknown or expired link.
	exec(`INSERT INTO paperboat.sessions(id,user_id,session_hash,csrf_hash,expires_at) VALUES($1,$2,$3,$4,$5)`, session+"_viewer", viewer, "hash_viewer_"+suffix, "csrf_viewer_"+suffix, now.Add(2*time.Hour))
	refused, _ := svc.Begin(ctx, BeginRequest{Host: host, ReturnPath: "/ok"})
	if _, err = svc.Issue(ctx, IssueRequest{TransactionID: refused.TransactionID, Principal: auth.Session{ID: session + "_viewer", UserID: viewer}}); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("viewer without access: %v, want ErrAccessDenied", err)
	}
	// Obtain a fresh issued handoff for the redemption race.
	begin, _ = svc.Begin(ctx, BeginRequest{Host: host, ReturnPath: "/ok"})
	issued, err := svc.Issue(ctx, IssueRequest{TransactionID: begin.TransactionID, Principal: auth.Session{ID: session, UserID: owner}})
	if err != nil {
		t.Fatal(err)
	}
	req := RedeemRequest{TransactionID: begin.TransactionID, State: begin.State, Handoff: issued.Handoff, Host: host}
	var wg sync.WaitGroup
	wg.Add(2)
	results := make(chan RedeemResult, 2)
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() { defer wg.Done(); r, e := svc.Redeem(ctx, req); results <- r; errs <- e }()
	}
	wg.Wait()
	close(results)
	close(errs)
	wins := 0
	var token string
	for r := range results {
		if r.Token != "" {
			wins++
			token = r.Token
		}
	}
	var redemptionErrors []error
	for e := range errs {
		redemptionErrors = append(redemptionErrors, e)
	}
	if wins != 1 {
		t.Fatalf("redeem winners=%d errors=%v", wins, redemptionErrors)
	}
	if a, e := svc.Authorize(ctx, AuthorizeRequest{Token: token, Host: host, ResourceKind: "tunnel", ResourceID: tunnel, RouteID: route}); e != nil || a.RouteGeneration != 7 || a.ExpiresAt.Sub(now) > 10*time.Second {
		t.Fatalf("authorize=%#v %v", a, e)
	}
	if _, e := svc.Authorize(ctx, AuthorizeRequest{Token: token, Host: "wrong." + host, ResourceKind: "tunnel", ResourceID: tunnel, RouteID: route}); e == nil {
		t.Fatal("wrong host authorized")
	}

	admitted, err := svc.Authorize(ctx, AuthorizeRequest{Token: token, Host: host, ResourceKind: "tunnel", ResourceID: tunnel, RouteID: route})
	if err != nil {
		t.Fatal(err)
	}
	exec(`UPDATE browser_access_sessions SET expires_at=$2 WHERE grant_id=$1`, admitted.GrantID, now.Add(-time.Second))
	if _, err := svc.Authorize(ctx, AuthorizeRequest{Token: token, Host: host, ResourceKind: "tunnel", ResourceID: tunnel, RouteID: route}); err == nil {
		t.Fatal("expired credential admitted a new browser request")
	}
	if _, err := svc.Cleanup(ctx, 256); err != nil {
		t.Fatal(err)
	}
	exec(`UPDATE sessions SET expires_at=$2 WHERE id=$1`, session, now.Add(-time.Second))
	if current, err := svc.AuthorizeGrant(ctx, admitted.GrantID, host, "tunnel", tunnel, route); err != nil || !current.ExpiresAt.After(now) {
		t.Fatal("admitted browser authority lost to credential expiry or cleanup")
	}
	exec(`UPDATE sessions SET expires_at=$2 WHERE id=$1`, session, now.Add(2*time.Hour))
	exec(`UPDATE paperboat.sessions SET version=version+1,rotated_at=$2 WHERE id=$1`, session, now)
	if _, err := svc.AuthorizeGrant(ctx, admitted.GrantID, host, "tunnel", tunnel, route); err == nil {
		t.Fatal("signed-out session retained admitted browser authority")
	}
	if _, e := svc.Authorize(ctx, AuthorizeRequest{Token: token, Host: host, ResourceKind: "tunnel", ResourceID: tunnel, RouteID: route}); e == nil {
		t.Fatal("rotated trusted session retained old grant")
	}
	rotatedBegin, _ := svc.Begin(ctx, BeginRequest{Host: host, ReturnPath: "/rotated"})
	if _, e := svc.Issue(ctx, IssueRequest{TransactionID: rotatedBegin.TransactionID, Principal: auth.Session{ID: session, UserID: owner}}); e != nil {
		t.Fatalf("current rotated session could not issue: %v", e)
	}
	exec(`UPDATE paperboat.tunnels SET generation=generation+1 WHERE id=$1`, tunnel)
	if _, e := svc.Authorize(ctx, AuthorizeRequest{Token: token, Host: host, ResourceKind: "tunnel", ResourceID: tunnel, RouteID: route}); e == nil {
		t.Fatal("changed generation authorized")
	}
	// Current explicit team grant is required and removal revokes immediately.
	exec(`INSERT INTO paperboat.teams(team_id,owner_account,generation) VALUES($1,$2,1)`, team, owner)
	exec(`INSERT INTO paperboat.team_members(team_id,account_id,membership_generation,role,active) VALUES($1,$2,1,'member',true),($1,$3,1,'member',true)`, team, owner, viewer)
	exec(`INSERT INTO paperboat.team_resource_bindings(team_id,resource_kind,resource_id,owner_account) VALUES($1,'tunnel',$2,$3)`, team, tunnel, owner)
	exec(`INSERT INTO paperboat.team_resource_grants(team_id,account_id,resource_kind,resource_id,permission,generation,active) VALUES($1,$2,'tunnel',$3,'use',1,true)`, team, viewer, tunnel)
	exec(`INSERT INTO paperboat.team_resource_grants(team_id,account_id,resource_kind,resource_id,permission,generation,active) VALUES($1,$2,'tunnel',$3,'use',1,true)`, team, owner, tunnel)
	exec(`INSERT INTO paperboat.billing_workspaces(id,team_id,state,product_code,quantity,period_start,period_end) VALUES($1,$2,'active','team-pro',2,$3,$4)`, "bw_"+team, team, now.Add(-time.Hour), now.Add(time.Hour))
	exec(`UPDATE paperboat.user_machines SET owner_team_id=$2 WHERE id=$1`, hostMachine, team)
	if _, err = svc.IssueMachineCredential(ctx, MachineCredentialRequest{AccountID: viewer, Host: host, ResourceKind: "tunnel", ResourceID: tunnel, RouteID: route, Action: "use", TTL: time.Minute}); err != nil {
		t.Fatalf("paid private team viewer denied: %v", err)
	}
	exec(`UPDATE paperboat.team_members SET active=false,membership_generation=membership_generation+1 WHERE team_id=$1 AND account_id=$2`, team, owner)
	if _, err = svc.IssueMachineCredential(ctx, MachineCredentialRequest{AccountID: owner, Host: host, ResourceKind: "tunnel", ResourceID: tunnel, RouteID: route, Action: "use", TTL: time.Minute}); !errors.Is(err, ErrNotAuthorized) {
		t.Fatalf("removed private publisher authorized: %v", err)
	}
	exec(`UPDATE paperboat.team_members SET active=true,membership_generation=membership_generation+1 WHERE team_id=$1 AND account_id=$2`, team, owner)
	exec(`DELETE FROM paperboat.browser_machine_credentials WHERE account_id=$1`, viewer)
	exec(`UPDATE paperboat.tunnels SET generation=5,access_mode='team' WHERE id=$1`, tunnel)
	exec(`INSERT INTO paperboat.teams(team_id,owner_account,generation) VALUES($1,$2,1)`, secondTeam, owner)
	exec(`INSERT INTO paperboat.team_members(team_id,account_id,membership_generation,role,active) VALUES($1,$2,1,'member',true)`, secondTeam, viewer)
	exec(`INSERT INTO paperboat.team_resource_bindings(team_id,resource_kind,resource_id,owner_account) VALUES($1,'tunnel',$2,$3)`, secondTeam, tunnel, owner)
	exec(`INSERT INTO paperboat.team_resource_grants(team_id,account_id,resource_kind,resource_id,permission,generation,active) VALUES($1,$2,'tunnel',$3,'use',1,true)`, secondTeam, viewer, tunnel)
	exec(`INSERT INTO paperboat.billing_workspaces(id,team_id,state,product_code,quantity,period_start,period_end) VALUES($1,$2,'active','team-pro',1,$3,$4)`, "bw_"+secondTeam, secondTeam, now.Add(-time.Hour), now.Add(time.Hour))
	if _, err = svc.IssueMachineCredential(ctx, MachineCredentialRequest{AccountID: viewer, Host: host, ResourceKind: "tunnel", ResourceID: tunnel, RouteID: route, Action: "use", TTL: 5 * time.Minute}); !errors.Is(err, ErrAmbiguousTeamAccess) {
		t.Fatalf("multiply-authorized browser access: %v", err)
	}
	var ambiguousCredentials int
	if err = store.SQL().QueryRowContext(ctx, `SELECT count(*) FROM paperboat.browser_machine_credentials WHERE account_id=$1`, viewer).Scan(&ambiguousCredentials); err != nil || ambiguousCredentials != 0 {
		t.Fatalf("ambiguous browser access persisted payer: count=%d err=%v", ambiguousCredentials, err)
	}
	exec(`UPDATE paperboat.team_resource_bindings SET active=false,generation=generation+1 WHERE team_id=$1 AND resource_kind='tunnel' AND resource_id=$2`, secondTeam, tunnel)
	machine, err := svc.IssueMachineCredential(ctx, MachineCredentialRequest{AccountID: viewer, Host: host, ResourceKind: "tunnel", ResourceID: tunnel, RouteID: route, Action: "use", TTL: 5 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = svc.AuthorizeMachineCredential(ctx, machine.Token, host, "tunnel", tunnel, route, "use"); err != nil {
		t.Fatal(err)
	}
	exec(`UPDATE paperboat.team_resource_grants SET active=false,generation=generation+1 WHERE team_id=$1 AND account_id=$2`, team, viewer)
	if _, err = svc.AuthorizeMachineCredential(ctx, machine.Token, host, "tunnel", tunnel, route, "use"); err == nil {
		t.Fatal("revoked team machine credential authorized")
	}
	exec(`UPDATE paperboat.browser_machine_credentials SET issued_at=$2,expires_at=$3 WHERE credential_id=$1`, machine.CredentialID, now.Add(-10*time.Minute), now.Add(-5*time.Minute))
	if _, err = svc.IssueMachineCredential(ctx, MachineCredentialRequest{AccountID: viewer, Host: host, ResourceKind: "tunnel", ResourceID: tunnel, RouteID: route, Action: "use", TTL: time.Minute}); err == nil {
		t.Fatal("revoked team issued another machine credential")
	}
	var retained int
	if err = store.SQL().QueryRowContext(ctx, `SELECT count(*) FROM paperboat.browser_machine_credentials WHERE credential_id=$1`, machine.CredentialID).Scan(&retained); err != nil || retained != 1 {
		t.Fatalf("cleanup removed unrevoked admission authority: count=%d err=%v", retained, err)
	}
}
