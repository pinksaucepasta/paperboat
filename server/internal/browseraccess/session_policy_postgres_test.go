package browseraccess

import (
	"context"
	"fmt"
	"github.com/pinksaucepasta/paperboat-server/internal/auth"
	"github.com/pinksaucepasta/paperboat-server/internal/config"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
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
	exec(`INSERT INTO paperboat.user_machines(id,user_id,environment_id,alias,description,platform,architecture,workspace_root,state,seat_state) VALUES($1,$2,$3,'session-host','Session host','linux','amd64','/workspace','offline','released')`, machine, owner, "env_"+suffix)
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
