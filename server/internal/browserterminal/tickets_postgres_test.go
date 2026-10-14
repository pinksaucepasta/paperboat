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
	if _, err := store.SQL().ExecContext(ctx, `UPDATE sessions SET revoked_at=now() WHERE id=$1`, browserSessionID); err != nil {
		t.Fatal(err)
	}
	if _, err := tickets.Consume(ctx, second.Value, second.Origin, terminalID); !errors.Is(err, ErrTicketInvalid) {
		t.Fatalf("revoked session ticket: %v", err)
	}
}
