package browserterminal

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/config"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
)

var testBrowserKeyDigest = strings.Repeat("A", 43)

func TestBrowserTerminalTicketIsSingleUseAndSessionBoundPostgres(t *testing.T) {
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
	if err := db.Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	suffix := fmt.Sprint(time.Now().UnixNano())
	userID, browserSessionID, machineID, terminalID := "usr_btt_"+suffix, "ses_btt_"+suffix, "mch_btt_"+suffix, "term_btt_"+suffix
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO users (id,workos_subject,primary_email,status) VALUES ($1,$2,$3,'active')`, userID, "workos_btt_"+suffix, "browser-ticket-"+suffix+"@example.test"); err != nil {
		t.Fatal(err)
	}
	defer store.SQL().ExecContext(ctx, `DELETE FROM users WHERE id=$1`, userID)
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO sessions (id,user_id,session_hash,csrf_hash,expires_at) VALUES ($1,$2,$3,$4,now()+interval '1 hour')`, browserSessionID, userID, "session_hash_"+suffix, "csrf_hash_"+suffix); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO user_machines (id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state) VALUES ($1,$2,$3,'browser-ticket-host','linux','amd64','/workspace','offline','released')`, machineID, userID, "env_btt_"+suffix); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO user_machine_terminal_sessions (id,user_machine_id,name,launch_cwd,owner_account) VALUES ($1,$2,'test','/workspace',$3)`, terminalID, machineID, userID); err != nil {
		t.Fatal(err)
	}
	tickets := TicketStore{DB: store}
	issue := func() Ticket {
		t.Helper()
		out, err := tickets.Issue(ctx, Ticket{UserID: userID, BrowserSessionID: browserSessionID, TerminalSessionID: terminalID, Origin: "https://dashboard.example.test", Role: "viewer", PolicyGeneration: 2, BrowserPublicKeySHA256: testBrowserKeyDigest})
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	first := issue()
	if _, err := tickets.Consume(ctx, first.Value, "https://other.example.test", terminalID); !errors.Is(err, ErrTicketInvalid) {
		t.Fatalf("wrong origin: %v", err)
	}
	if _, err := tickets.Consume(ctx, first.Value, "https://dashboard.example.test", "other_terminal"); !errors.Is(err, ErrTicketInvalid) {
		t.Fatalf("wrong terminal: %v", err)
	}
	used, err := tickets.Consume(ctx, first.Value, first.Origin, terminalID)
	if err != nil || used.UserID != userID || used.Role != "viewer" || used.PolicyGeneration != 2 || used.BrowserPublicKeySHA256 != testBrowserKeyDigest {
		t.Fatalf("consumed ticket = %#v, %v", used, err)
	}
	if _, err := tickets.Consume(ctx, first.Value, first.Origin, terminalID); !errors.Is(err, ErrTicketInvalid) {
		t.Fatalf("replayed ticket: %v", err)
	}
	expired := issue()
	if _, err := store.SQL().ExecContext(ctx, `UPDATE browser_terminal_tickets SET created_at=now()-interval '1 minute',expires_at=now()-interval '1 second' WHERE browser_session_id=$1 AND consumed_at IS NULL`, browserSessionID); err != nil {
		t.Fatal(err)
	}
	if _, err := tickets.Consume(ctx, expired.Value, expired.Origin, terminalID); !errors.Is(err, ErrTicketInvalid) {
		t.Fatalf("expired ticket: %v", err)
	}
	concurrent := make(chan error, 16)
	for i := 0; i < cap(concurrent); i++ {
		go func() {
			_, issueErr := tickets.Issue(ctx, Ticket{UserID: userID, BrowserSessionID: browserSessionID, TerminalSessionID: terminalID, Origin: "https://dashboard.example.test", Role: "viewer", PolicyGeneration: 2, BrowserPublicKeySHA256: testBrowserKeyDigest})
			concurrent <- issueErr
		}()
	}
	accepted := 0
	for i := 0; i < cap(concurrent); i++ {
		issueErr := <-concurrent
		if issueErr == nil {
			accepted++
		} else if !errors.Is(issueErr, ErrTicketInvalid) {
			t.Fatalf("concurrent ticket issue: %v", issueErr)
		}
	}
	if accepted != 8 {
		t.Fatalf("concurrent issue accepted %d tickets, want exactly 8", accepted)
	}
	if _, err := store.SQL().ExecContext(ctx, `DELETE FROM browser_terminal_tickets WHERE browser_session_id=$1 AND consumed_at IS NULL`, browserSessionID); err != nil {
		t.Fatal(err)
	}
	second := issue()
	for i := 0; i < 7; i++ {
		issue()
	}
	if _, err := tickets.Issue(ctx, Ticket{UserID: userID, BrowserSessionID: browserSessionID, TerminalSessionID: terminalID, Origin: "https://dashboard.example.test", Role: "viewer", PolicyGeneration: 2, BrowserPublicKeySHA256: testBrowserKeyDigest}); !errors.Is(err, ErrTicketInvalid) {
		t.Fatalf("unconsumed ticket capacity: %v", err)
	}
	if _, err := store.SQL().ExecContext(ctx, `UPDATE sessions SET expires_at=now()-interval '1 second' WHERE id=$1`, browserSessionID); err != nil {
		t.Fatal(err)
	}
	if active, err := tickets.SessionActive(ctx, userID, browserSessionID); err != nil || !active {
		t.Fatal("admitted terminal lost to browser credential expiry")
	}
	if _, err := tickets.Consume(ctx, second.Value, second.Origin, terminalID); !errors.Is(err, ErrTicketInvalid) {
		t.Fatal("expired browser session admitted a new attachment")
	}
	if _, err := store.SQL().ExecContext(ctx, `UPDATE sessions SET revoked_at=now() WHERE id=$1`, browserSessionID); err != nil {
		t.Fatal(err)
	}
	if active, err := tickets.SessionActive(ctx, userID, browserSessionID); err != nil || active {
		t.Fatal("revoked browser session retained")
	}
	if _, err := tickets.Consume(ctx, second.Value, second.Origin, terminalID); !errors.Is(err, ErrTicketInvalid) {
		t.Fatalf("revoked session ticket: %v", err)
	}
}

func TestBrowserInspectorTicketIsBoundToItsCredentialPostgres(t *testing.T) {
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
	if err := db.Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	suffix := fmt.Sprint(time.Now().UnixNano())
	userID, browserSessionID, credentialID := "usr_bit_"+suffix, "ses_bit_"+suffix, "credential_bit_"+suffix
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO users (id,workos_subject,primary_email,status) VALUES ($1,$1,$1||'@example.test','active')`, userID)
	defer store.SQL().ExecContext(ctx, `DELETE FROM users WHERE id=$1`, userID)
	defer store.SQL().ExecContext(ctx, `DELETE FROM inspector_credentials WHERE credential_id=$1`, credentialID)
	exec(`INSERT INTO sessions (id,user_id,session_hash,csrf_hash,expires_at) VALUES ($1,$2,$3,$4,now()+interval '1 hour')`, browserSessionID, userID, "session_hash_"+suffix, "csrf_hash_"+suffix)
	exec(`INSERT INTO inspector_credentials (credential_id,token_hash,account_id,resource_kind,resource_id,route_id,action,resource_generation,route_generation,target_generation,owner_account_id,issued_at,expires_at) VALUES ($1,$2,$3,'preview','prv_x','prv_x','inspect',1,1,1,$3,now(),now()+interval '4 minutes')`, credentialID, []byte(fmt.Sprintf("%-32.32s", suffix)), userID)
	tickets := TicketStore{DB: store}
	base := Ticket{UserID: userID, BrowserSessionID: browserSessionID, InspectorCredentialID: credentialID, Origin: "https://dashboard.example.test", Role: RoleInspector, PolicyGeneration: 1, BrowserPublicKeySHA256: testBrowserKeyDigest}
	issued, err := tickets.Issue(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	// An inspector ticket never opens a terminal: the terminal-bound consume
	// path cannot redeem it.
	if _, err := tickets.Consume(ctx, issued.Value, issued.Origin, "term_any"); !errors.Is(err, ErrTicketInvalid) {
		t.Fatalf("inspector ticket redeemed for a terminal: %v", err)
	}
	used, err := tickets.ConsumeEdge(ctx, issued.Value, issued.Origin)
	if err != nil || used.Role != RoleInspector || used.InspectorCredentialID != credentialID || used.TerminalSessionID != "" || used.UserID != userID {
		t.Fatalf("consumed inspector ticket = %#v, %v", used, err)
	}
	if _, err := tickets.ConsumeEdge(ctx, issued.Value, issued.Origin); !errors.Is(err, ErrTicketInvalid) {
		t.Fatalf("replayed inspector ticket: %v", err)
	}
	for name, mutate := range map[string]func(*Ticket){
		"terminal session too": func(in *Ticket) { in.TerminalSessionID = "term_x" },
		"no credential":        func(in *Ticket) { in.InspectorCredentialID = "" },
		"terminal role":        func(in *Ticket) { in.Role = "owner" },
	} {
		invalid := base
		mutate(&invalid)
		if _, err := tickets.Issue(ctx, invalid); !errors.Is(err, ErrTicketInvalid) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	// The database enforces the same shape.
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO browser_terminal_tickets (token_hash,user_id,browser_session_id,origin,role,policy_generation,browser_public_key_sha256,expires_at) VALUES ($1,$2,$3,'https://dashboard.example.test','inspector',1,$4,now()+interval '30 seconds')`, make([]byte, 32), userID, browserSessionID, testBrowserKeyDigest); err == nil {
		t.Fatal("database accepted an inspector ticket without a credential")
	}
	// Revoking (deleting) the credential removes its tickets.
	second, err := tickets.Issue(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	exec(`DELETE FROM inspector_credentials WHERE credential_id=$1`, credentialID)
	if _, err := tickets.ConsumeEdge(ctx, second.Value, second.Origin); !errors.Is(err, ErrTicketInvalid) {
		t.Fatalf("ticket outlived its credential: %v", err)
	}
}
