package browseraccess

import (
	"context"
	"fmt"
	"github.com/pinksaucepasta/paperboat/server/internal/auth"
	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"os"
	"testing"
	"time"
)

func TestConfiguredResourceSessionPostgres(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
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
	if err = db.Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	suffix := fmt.Sprint(time.Now().UnixNano())
	owner, session, tunnel, route, machine := "usr_cfg_"+suffix, "ses_cfg_"+suffix, "tun_cfg_"+suffix, "rte_cfg_"+suffix, "host_cfg_"+suffix
	host := "00000000-0000-8000-8000-" + suffix[len(suffix)-12:] + ".tunnels.example.test"
	start := time.Now().UTC().Truncate(time.Microsecond)
	now := start
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@test.invalid','active')`, owner)
	defer store.SQL().ExecContext(ctx, `DELETE FROM paperboat.users WHERE id=$1`, owner)
	exec(`INSERT INTO paperboat.user_machines(id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state) VALUES($1,$2,$3,'session-host','linux','amd64','/workspace','offline','released')`, machine, owner, "env_"+suffix)
	exec(`INSERT INTO paperboat.sessions(id,user_id,session_hash,csrf_hash,expires_at) VALUES($1,$2,$3,$4,$5)`, session, owner, "hash_"+suffix, "csrf_"+suffix, start.Add(time.Hour))
	exec(`INSERT INTO paperboat.tunnels(id,account_id,name,desired_state,access_mode,generation,stable_endpoint_id,stable_endpoint,created_by_host_id,created_by_actor_id,created_at,updated_at) VALUES($1,$2,$1,'active','private',3,$3,$4,$5,$2,$6,$6)`, tunnel, owner, "endpoint_00000000-0000-4000-8000-"+suffix[len(suffix)-12:], "https://"+host, machine, start)
	exec(`INSERT INTO paperboat.tunnel_routes(id,tunnel_id,name,protocol,match_type,origin_scheme,origin_address,generation,desired_state,created_by_actor_id,updated_by_actor_id,created_at,updated_at) VALUES($1,$2,'web','http','catch_all','http','127.0.0.1:3000',7,'active',$3,$3,$4,$4)`, route, tunnel, owner, start)
	s, err := NewService(store, "https://login.example.test")
	if err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return now }
	p := config.DefaultBrowserSessions()
	p.ResourceAbsoluteLifetime = 10 * time.Minute
	p.ResourceIdleLifetime = 4 * time.Minute
	if err = s.ConfigureSessions(p); err != nil {
		t.Fatal(err)
	}
	mint := func() RedeemResult {
		t.Helper()
		begin, e := s.Begin(ctx, BeginRequest{Host: host, ReturnPath: "/"})
		if e != nil {
			t.Fatal(e)
		}
		issued, e := s.Issue(ctx, IssueRequest{TransactionID: begin.TransactionID, Principal: auth.Session{ID: session, UserID: owner}})
		if e != nil {
			t.Fatal(e)
		}
		r, e := s.Redeem(ctx, RedeemRequest{TransactionID: begin.TransactionID, State: begin.State, Handoff: issued.Handoff, Host: host})
		if e != nil {
			t.Fatal(e)
		}
		return r
	}
	grant := mint()
	if !grant.ExpiresAt.Equal(start.Add(4 * time.Minute)) {
		t.Fatal("configured initial idle deadline ignored")
	}
	authorize := func(token string) (Authorization, error) {
		return s.Authorize(ctx, AuthorizeRequest{Token: token, Host: host, ResourceKind: "tunnel", ResourceID: tunnel, RouteID: route})
	}
	for _, minute := range []int{3, 6, 9} {
		now = start.Add(time.Duration(minute) * time.Minute)
		a, e := authorize(grant.Token)
		if e != nil {
			t.Fatal(e)
		}
		want := minTime(now.Add(4*time.Minute), start.Add(10*time.Minute))
		if !a.SessionExpiresAt.Equal(want) {
			t.Fatalf("idle/absolute deadline=%v want=%v", a.SessionExpiresAt, want)
		}
	}
	now = start.Add(10 * time.Minute)
	if _, e := authorize(grant.Token); e == nil {
		t.Fatal("absolute expiry bypassed by activity")
	}
	idle := mint()
	now = now.Add(4 * time.Minute)
	if _, e := authorize(idle.Token); e == nil {
		t.Fatal("idle expiry ignored")
	}
	exec(`UPDATE paperboat.sessions SET expires_at=$2 WHERE id=$1`, session, now.Add(2*time.Minute))
	capped := mint()
	if !capped.ExpiresAt.Equal(now.Add(2 * time.Minute)) {
		t.Fatal("parent lifetime cap ignored")
	}
	exec(`UPDATE paperboat.sessions SET version=version+1 WHERE id=$1`, session)
	if _, e := authorize(capped.Token); e == nil {
		t.Fatal("parent session rotation did not revoke child")
	}
}

// Production 2026-11-05: a private durable tunnel answered its own signed-in owner 403. Browser sessions
// fenced on tunnels.generation, which every ordinary tunnel edit advances, so the next request after an
// edit refused the owner's session (404 to the edge, rendered as No access). The fence is
// access_generation: an edit keeps the owner in, an access-mode change still revokes, another account stays out.
func TestTunnelOwnerSessionSurvivesAnOrdinaryTunnelEditPostgres(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
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
	if err = db.Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	suffix := fmt.Sprint(time.Now().UnixNano())
	owner, other, session, otherSession := "usr_edit_"+suffix, "usr_other_"+suffix, "ses_edit_"+suffix, "ses_other_"+suffix
	tunnel, route, machine := "tun_edit_"+suffix, "rte_edit_"+suffix, "host_edit_"+suffix
	host := "00000000-0000-8000-8000-" + suffix[len(suffix)-12:] + ".tunnels.example.test"
	now := time.Now().UTC().Truncate(time.Microsecond)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	for _, user := range []string{owner, other} {
		exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@test.invalid','active')`, user)
		defer store.SQL().ExecContext(ctx, `DELETE FROM paperboat.users WHERE id=$1`, user)
	}
	exec(`INSERT INTO paperboat.user_machines(id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state) VALUES($1,$2,$3,'edit-host','linux','amd64','/workspace','offline','released')`, machine, owner, "env_"+suffix)
	exec(`INSERT INTO paperboat.sessions(id,user_id,session_hash,csrf_hash,expires_at) VALUES($1,$2,$3,$4,$5)`, session, owner, "hash_"+suffix, "csrf_"+suffix, now.Add(time.Hour))
	exec(`INSERT INTO paperboat.sessions(id,user_id,session_hash,csrf_hash,expires_at) VALUES($1,$2,$3,$4,$5)`, otherSession, other, "hash_o_"+suffix, "csrf_o_"+suffix, now.Add(time.Hour))
	exec(`INSERT INTO paperboat.tunnels(id,account_id,name,desired_state,access_mode,generation,stable_endpoint_id,stable_endpoint,created_by_host_id,created_by_actor_id,created_at,updated_at) VALUES($1,$2,$1,'active','private',1,$3,$4,$5,$2,$6,$6)`, tunnel, owner, "endpoint_00000000-0000-4000-8000-"+suffix[len(suffix)-12:], "https://"+host, machine, now)
	exec(`INSERT INTO paperboat.tunnel_routes(id,tunnel_id,name,protocol,match_type,origin_scheme,origin_address,generation,desired_state,created_by_actor_id,updated_by_actor_id,created_at,updated_at) VALUES($1,$2,'web','http','catch_all','http','127.0.0.1:3000',1,'active',$3,$3,$4,$4)`, route, tunnel, owner, now)
	s, err := NewService(store, "https://login.example.test")
	if err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return now }
	issue := func(sessionID, user string) (BeginResult, IssueResult) {
		t.Helper()
		begin, e := s.Begin(ctx, BeginRequest{Host: host, ReturnPath: "/"})
		if e != nil {
			t.Fatal(e)
		}
		issued, e := s.Issue(ctx, IssueRequest{TransactionID: begin.TransactionID, Principal: auth.Session{ID: sessionID, UserID: user}})
		if e != nil {
			t.Fatal(e)
		}
		return begin, issued
	}
	begin, issued := issue(session, owner)
	if issued.Denied {
		t.Fatal("the owner was denied their own private tunnel")
	}
	grant, err := s.Redeem(ctx, RedeemRequest{TransactionID: begin.TransactionID, State: begin.State, Handoff: issued.Handoff, Host: host})
	if err != nil {
		t.Fatal(err)
	}
	authorize := func() (Authorization, error) {
		return s.Authorize(ctx, AuthorizeRequest{Token: grant.Token, Host: host, ResourceKind: "tunnel", ResourceID: tunnel, RouteID: route})
	}
	exec(`UPDATE paperboat.tunnels SET generation=generation+1,name='renamed' WHERE id=$1`, tunnel)
	a, err := authorize()
	if err != nil {
		t.Fatalf("an ordinary tunnel edit refused the owner's session: %v", err)
	}
	// The edge's ingress decision binds the tunnel's current generation; the authorization must report it.
	if a.ResourceGeneration != 2 {
		t.Fatalf("authorization resource generation = %d, want the tunnel's current generation 2", a.ResourceGeneration)
	}
	if _, otherIssue := issue(otherSession, other); !otherIssue.Denied {
		t.Fatal("another account was let into a private tunnel")
	}
	// The machine re-checks the edge's decision by grant (/v1/browser-access/ingress/authorize). An engine
	// tunnel's decision names its runtime carrier as tunnel_id; the tunnel comes from the route instead
	// (production 2026-11-05: the owner got 502 when the carrier id was compared with the tunnel).
	if got, err := s.TunnelForRoute(ctx, route); err != nil || got != tunnel {
		t.Fatalf("TunnelForRoute = %q %v, want %q", got, err, tunnel)
	}
	if _, err := s.AuthorizeGrant(ctx, a.GrantID, host, "tunnel", tunnel, route); err != nil {
		t.Fatalf("the owner's grant was refused for its own tunnel: %v", err)
	}
	if _, err := s.AuthorizeGrant(ctx, a.GrantID, host, "tunnel", "route_runtime_carrier_"+suffix, route); err == nil {
		t.Fatal("a grant was accepted for a resource it is not bound to")
	}
	exec(`UPDATE paperboat.tunnels SET access_mode='public' WHERE id=$1`, tunnel)
	if _, err := authorize(); err == nil {
		t.Fatal("an access-mode change did not revoke the browser session")
	}
}
