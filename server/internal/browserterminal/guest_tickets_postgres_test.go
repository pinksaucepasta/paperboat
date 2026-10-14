package browserterminal

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pinksaucepasta/paperboat-server/internal/config"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
)

func TestGuestTicketsPostgres(t *testing.T) {
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
	user, accountSession, machine, terminal, guest := "usr_guest_"+suffix, "ses_guest_"+suffix, "mch_guest_"+suffix, "term_guest_"+suffix, "guest_"+suffix
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO users (id,workos_subject,primary_email,status) VALUES ($1,$2,$3,'active')`, user, "workos_guest_"+suffix, "guest-test-"+suffix+"@example.test")
	defer store.SQL().ExecContext(ctx, `DELETE FROM users WHERE id=$1`, user)
	exec(`INSERT INTO sessions (id,user_id,session_hash,csrf_hash,expires_at) VALUES ($1,$2,$3,$4,now()+interval '1 hour')`, accountSession, user, "session_guest_"+suffix, "csrf_guest_"+suffix)
	exec(`INSERT INTO user_machines (id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state) VALUES ($1,$2,$3,'guest-host','linux','amd64','/workspace','offline','released')`, machine, user, "env_guest_"+suffix)
	exec(`INSERT INTO user_machine_terminal_sessions (id,user_machine_id,name,launch_cwd,owner_account) VALUES ($1,$2,'guest-test','/workspace',$3)`, terminal, machine, user)
	codeHash := sha256.Sum256([]byte("test-code-" + suffix))
	guestHash := sha256.Sum256([]byte("test-guest-session-" + suffix))
	exec(`INSERT INTO terminal_code_access (terminal_session_id,generation,enabled,code_hash,role,expires_at) VALUES ($1,2,true,$2,'viewer',now()+interval '1 hour')`, terminal, codeHash[:])
	exec(`INSERT INTO terminal_guest_sessions (id,token_hash,terminal_session_id,code_generation,display_name,expires_at) VALUES ($1,$2,$3,2,'Guest',now()+interval '1 hour')`, guest, guestHash[:], terminal)
	tickets := TicketStore{DB: store}
	in := Ticket{GuestSessionID: guest, TerminalSessionID: terminal, Origin: "https://dashboard.example.test", Role: "viewer", PolicyGeneration: 2, BrowserPublicKeySHA256: testBrowserKeyDigest}
	issue := func() Ticket {
		t.Helper()
		out, err := tickets.Issue(ctx, in)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	first := issue()
	for _, wrong := range []struct{ origin, terminal string }{{"https://other.example.test", terminal}, {in.Origin, "other_terminal"}} {
		if _, err := tickets.Consume(ctx, first.Value, wrong.origin, wrong.terminal); !errors.Is(err, ErrTicketInvalid) {
			t.Fatalf("wrong resource/origin admitted: %v", err)
		}
	}
	used, err := tickets.Consume(ctx, first.Value, in.Origin, terminal)
	if err != nil || used.GuestSessionID != guest || used.UserID != "" || used.BrowserSessionID != "" || used.Role != "viewer" || used.PolicyGeneration != 2 || used.BrowserPublicKeySHA256 != testBrowserKeyDigest {
		t.Fatalf("guest ticket binding: %+v %v", used, err)
	}
	if _, err = tickets.ConsumeEdge(ctx, first.Value, in.Origin); !errors.Is(err, ErrTicketInvalid) {
		t.Fatalf("guest ticket replay: %v", err)
	}
	clearPending := func() {
		exec(`DELETE FROM browser_terminal_tickets WHERE guest_session_id=$1 AND consumed_at IS NULL`, guest)
	}
	for _, test := range []struct{ name, change, restore string }{
		{"ticket expiry", `UPDATE browser_terminal_tickets SET created_at=now()-interval '1 minute',expires_at=now()-interval '1 second' WHERE guest_session_id=$1 AND consumed_at IS NULL`, ``},
		{"guest expiry", `UPDATE terminal_guest_sessions SET created_at=now()-interval '1 minute',expires_at=now()-interval '1 second' WHERE id=$1`, `UPDATE terminal_guest_sessions SET expires_at=now()+interval '1 hour' WHERE id=$1`},
		{"code reset", `UPDATE terminal_code_access SET generation=3 WHERE terminal_session_id=$1`, `UPDATE terminal_code_access SET generation=2 WHERE terminal_session_id=$1`},
		{"code expiry", `UPDATE terminal_code_access SET expires_at=now()-interval '1 second' WHERE terminal_session_id=$1`, `UPDATE terminal_code_access SET expires_at=now()+interval '1 hour' WHERE terminal_session_id=$1`},
		{"code stop", `UPDATE terminal_code_access SET enabled=false,code_hash=NULL,expires_at=NULL WHERE terminal_session_id=$1`, `UPDATE terminal_code_access SET enabled=true,code_hash=$2,expires_at=now()+interval '1 hour' WHERE terminal_session_id=$1`},
	} {
		t.Run(test.name, func(t *testing.T) {
			clearPending()
			out := issue()
			id := terminal
			if test.name == "ticket expiry" || test.name == "guest expiry" {
				id = guest
			}
			exec(test.change, id)
			if _, err := tickets.ConsumeEdge(ctx, out.Value, in.Origin); !errors.Is(err, ErrTicketInvalid) {
				t.Fatalf("invalidated guest ticket admitted: %v", err)
			}
			if test.name != "ticket expiry" {
				if active, err := tickets.GuestSessionActive(ctx, guest); err != nil || active {
					t.Fatalf("invalid guest session active: %v %v", active, err)
				}
				if _, err := tickets.Issue(ctx, in); !errors.Is(err, ErrTicketInvalid) {
					t.Fatalf("invalid guest issued fresh ticket: %v", err)
				}
			}
			if test.restore != "" {
				if test.name == "code stop" {
					exec(test.restore, id, codeHash[:])
				} else {
					exec(test.restore, id)
				}
			}
		})
	}
	clearPending()
	concurrent := make(chan error, 16)
	for i := 0; i < cap(concurrent); i++ {
		go func() { _, err := tickets.Issue(ctx, in); concurrent <- err }()
	}
	accepted := 0
	var unexpected error
	for i := 0; i < cap(concurrent); i++ {
		err := <-concurrent
		if err == nil {
			accepted++
		} else if !errors.Is(err, ErrTicketInvalid) {
			unexpected = err
		}
	}
	if unexpected != nil || accepted != 8 {
		t.Fatalf("concurrent guest pending limit: accepted=%d err=%v", accepted, unexpected)
	}
	clearPending()
	for _, test := range []struct{ name, user, session, guest, role string }{
		{"guest owner", "", "", guest, "owner"},
		{"mixed account guest", user, accountSession, guest, "viewer"},
		{"no principal", "", "", "", "viewer"},
		{"partial account", user, "", "", "viewer"},
		{"missing guest FK", "", "", "missing_guest_" + suffix, "viewer"},
	} {
		t.Run(test.name, func(t *testing.T) {
			hash := sha256.Sum256([]byte(test.name + suffix))
			_, err := store.SQL().ExecContext(ctx, `INSERT INTO browser_terminal_tickets (token_hash,user_id,browser_session_id,guest_session_id,terminal_session_id,origin,role,policy_generation,browser_public_key_sha256,expires_at) VALUES ($1,nullif($2,''),nullif($3,''),nullif($4,''),$5,$6,$7,2,$8,now()+interval '30 seconds')`, hash[:], test.user, test.session, test.guest, terminal, in.Origin, test.role, testBrowserKeyDigest)
			if err == nil {
				t.Fatal("database accepted invalid guest principal")
			}
			wantCode := "23514"
			if test.name == "missing guest FK" {
				wantCode = "23503"
			}
			var postgresError *pgconn.PgError
			if !errors.As(err, &postgresError) || postgresError.Code != wantCode {
				t.Fatalf("expected principal constraint %s, got %v", wantCode, err)
			}
		})
	}
}
