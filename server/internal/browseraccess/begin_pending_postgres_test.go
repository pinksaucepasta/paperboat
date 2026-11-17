package browseraccess

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/auth"
	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

// A host under the machine-service domain with no route yet must start a sign-in (it was a 503 check violation).
func TestBeginPendingMachineServiceHostPostgres(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("set PAPERBOAT_TEST_DATABASE_DSN")
	}
	if e := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); e != nil {
		t.Fatal(e)
	}
	store, e := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if e != nil {
		t.Fatal(e)
	}
	defer store.Close()
	ctx := context.Background()
	if e := db.Migrate(ctx, store); e != nil {
		t.Fatal(e)
	}
	svc, e := NewService(store, "https://console.example.test")
	if e != nil {
		t.Fatal(e)
	}
	svc.MachineServiceDomain = "preview.example.test"
	out, e := svc.Begin(ctx, BeginRequest{Host: "no-route-yet.preview.example.test", ReturnPath: "/"})
	if e != nil || out.TransactionID == "" {
		t.Fatalf("begin: %+v %v", out, e)
	}
	defer store.SQL().ExecContext(ctx, `DELETE FROM paperboat.browser_access_transactions WHERE transaction_id=$1`, out.TransactionID)
}

// A sign-in burst at one resource refuses only that resource (it was one server-wide cap: any burst refused everyone).
func TestBeginCapacityIsPerResourcePostgres(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("set PAPERBOAT_TEST_DATABASE_DSN")
	}
	if e := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); e != nil {
		t.Fatal(e)
	}
	store, e := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if e != nil {
		t.Fatal(e)
	}
	defer store.Close()
	ctx := context.Background()
	if e := db.Migrate(ctx, store); e != nil {
		t.Fatal(e)
	}
	svc, e := NewService(store, "https://console.example.test")
	if e != nil {
		t.Fatal(e)
	}
	svc.MachineServiceDomain = "preview.example.test"
	busy, quiet := "busy.preview.example.test", "quiet.preview.example.test"
	defer store.SQL().ExecContext(ctx, `DELETE FROM paperboat.browser_access_transactions WHERE hostname IN ($1,$2)`, busy, quiet)
	if _, e := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.browser_access_transactions(transaction_id,state_hash,hostname,return_path,resource_kind,resource_id,route_id,created_at,expires_at)
SELECT 'transaction_capfill_'||g, sha256(('capfill'||g)::bytea), $1, '/', 'machine_service_pending', $1, $1, now(), now()+interval '10 minutes'
FROM generate_series(1,$2::int) g`, busy, pendingBeginsPerResource); e != nil {
		t.Fatal(e)
	}
	if _, e := svc.Begin(ctx, BeginRequest{Host: busy, ReturnPath: "/"}); e != ErrCapacity {
		t.Fatalf("busy resource begin: %v, want ErrCapacity", e)
	}
	if out, e := svc.Begin(ctx, BeginRequest{Host: quiet, ReturnPath: "/"}); e != nil || out.TransactionID == "" {
		t.Fatalf("other resource begin: %+v %v", out, e)
	}
}

// Anyone can sign up, so Begin must not tell a name no resource owns from one an account may not open: both start a
// sign-in, and the signed-in account is refused with the one denied answer (a 404 here let any visitor list names).
func TestBeginUnknownNameIsADeniedSignInPostgres(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("set PAPERBOAT_TEST_DATABASE_DSN")
	}
	if e := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); e != nil {
		t.Fatal(e)
	}
	store, e := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if e != nil {
		t.Fatal(e)
	}
	defer store.Close()
	ctx := context.Background()
	if e := db.Migrate(ctx, store); e != nil {
		t.Fatal(e)
	}
	suffix := fmt.Sprint(time.Now().UnixNano())
	user, session, machine, env := "usr_decoy_"+suffix, "ses_decoy_"+suffix, "host_decoy_"+suffix, "env_decoy_"+suffix
	reserved := "reserved-" + suffix + ".custom.example.test"
	exec := func(q string, args ...any) {
		t.Helper()
		if _, e := store.SQL().ExecContext(ctx, q, args...); e != nil {
			t.Fatal(e)
		}
	}
	t.Cleanup(func() {
		_, _ = store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.browser_access_transactions WHERE account_id=$1 OR hostname LIKE '%'||$2||'.%'`, user, suffix)
		_, _ = store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.lazy_access_policies WHERE account_id=$1`, user)
		_, _ = store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.user_machines WHERE id=$1`, machine)
		_, _ = store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.sessions WHERE id=$1`, session)
		_, _ = store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.users WHERE id=$1`, user)
	})
	exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@test.invalid','active')`, user)
	exec(`INSERT INTO paperboat.user_machines(id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state) VALUES($1,$2,$3,'decoy-host','linux','amd64','/workspace','offline','released')`, machine, user, env)
	exec(`INSERT INTO paperboat.sessions(id,user_id,session_hash,csrf_hash,expires_at) VALUES($1,$2,$3,$4,$5)`, session, user, "hash_"+suffix, "csrf_"+suffix, time.Now().Add(2*time.Hour))
	// An expired policy keeps owning its name: a revoked app must look like a name that never existed.
	exec(`INSERT INTO paperboat.lazy_access_policies(id,hostname,account_id,environment_id,machine_id,installation_generation,generation,target_scheme,target_address,access_mode,ownership_mode,expires_at,authorization_source) VALUES($1,$2,$3,$4,$5,1,1,'http','127.0.0.1:80','private','persistent_port',now()-interval '1 hour','explicit')`, "lz_decoy_"+suffix, reserved, user, env, machine)
	svc, e := NewService(store, "https://console.example.test")
	if e != nil {
		t.Fatal(e)
	}
	svc.MachineServiceDomain = "preview.example.test"
	for _, host := range []string{"nothing-" + suffix + ".custom.example.test", "a.b-" + suffix + ".preview.example.test", reserved} {
		begin, e := svc.Begin(ctx, BeginRequest{Host: host, ReturnPath: "/"})
		if e != nil || begin.TransactionID == "" {
			t.Fatalf("%s: begin %+v %v, want a sign-in", host, begin, e)
		}
		issue, e := svc.Issue(ctx, IssueRequest{TransactionID: begin.TransactionID, Principal: auth.Session{ID: session, UserID: user}})
		if e != nil || !issue.Denied || issue.Handoff == "" {
			t.Fatalf("%s: issue %+v %v, want a denied handoff", host, issue, e)
		}
		if _, e = svc.Redeem(ctx, RedeemRequest{TransactionID: begin.TransactionID, State: begin.State, Handoff: issue.Handoff, Host: host}); !errors.Is(e, ErrAccessDenied) {
			t.Fatalf("%s: redeem %v, want ErrAccessDenied", host, e)
		}
	}
}
