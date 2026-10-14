package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/audit"
	"github.com/pinksaucepasta/paperboat-server/internal/auth"
	"github.com/pinksaucepasta/paperboat-server/internal/browserterminal"
	"github.com/pinksaucepasta/paperboat-server/internal/config"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
	"github.com/pinksaucepasta/paperboat-server/internal/testauth"
	"github.com/pinksaucepasta/paperboat-server/internal/usermachines"
)

func TestTerminalCodeBrowserAuthorizationPostgres(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("isolated PostgreSQL required")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	store, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if err = db.Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	writer := audit.NewWriter(store)
	accounts := auth.NewService(store, writer, testauth.FakeWorkOSVerifier{}, []string{"terminal-code-session"}, true, "https://login.pprbt.dev")
	machines := usermachines.New(store, writer, usermachines.Policy{}, nil)
	gateway := &browserterminal.Gateway{Tickets: browserterminal.TicketStore{DB: store}, Authorizer: browserterminal.MachineAuthorizer{Service: machines}, AllowedOrigins: []string{"https://login.pprbt.dev"}}
	router := NewRouter(Options{Config: config.Default(), Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Auth: accounts, DeviceAuth: auth.NewDeviceService(store, writer, config.Default().CLIAuth, []string{"terminal-code-machine"}), Machines: machines, BrowserTerminal: gateway})
	suffix := fmt.Sprint(time.Now().UnixNano())
	ownerEmail := "code-http-owner-" + suffix + "@example.test"
	otherEmail := "code-http-other-" + suffix + "@example.test"
	ownerCookies := loginCookies(t, authTestOriginHandler{next: router}, "workos_code_http_owner_"+suffix+":"+ownerEmail+":Code Owner")
	otherCookies := loginCookies(t, authTestOriginHandler{next: router}, "workos_code_http_other_"+suffix+":"+otherEmail+":Other")
	var ownerID string
	if err = store.SQL().QueryRowContext(ctx, `SELECT id FROM users WHERE primary_email=$1`, ownerEmail).Scan(&ownerID); err != nil {
		t.Fatal(err)
	}
	machine, terminal, second := "mch_code_http_"+suffix, "term_code_http_"+suffix, "term_code_http_second_"+suffix
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		store.SQL().ExecContext(ctx, `DELETE FROM user_machine_terminal_sessions WHERE id IN ($1,$2)`, terminal, second)
		store.SQL().ExecContext(ctx, `DELETE FROM user_machines WHERE id=$1`, machine)
		store.SQL().ExecContext(ctx, `DELETE FROM users WHERE primary_email IN ($1,$2)`, ownerEmail, otherEmail)
	})
	exec(`INSERT INTO user_machines(id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state) VALUES($1,$2,$3,'code-api-host','linux','amd64','/workspace','online','occupied')`, machine, ownerID, "env_code_http_"+suffix)
	exec(`INSERT INTO user_machine_terminal_sessions(id,user_machine_id,name,launch_cwd,owner_account,desired_state,runtime_state) VALUES($1,$2,'test','/workspace',$3,'open','running'),($4,$2,'second','/workspace',$3,'open','running')`, terminal, machine, ownerID, second)
	call := func(method, path string, body any, cookies []*http.Cookie, origin, csrf string) *httptest.ResponseRecorder {
		t.Helper()
		encoded, _ := json.Marshal(body)
		req := httptest.NewRequest(method, path, bytes.NewReader(encoded))
		req.Header.Set("Content-Type", "application/json")
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		if csrf != "" {
			req.Header.Set(auth.CSRFHeaderName, csrf)
		}
		addCookies(req, cookies)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}
	path := "/v1/terminal-sessions/" + terminal
	check := func(rec *httptest.ResponseRecorder, status int) {
		t.Helper()
		if rec.Code != status {
			t.Fatalf("status %d want %d: %s", rec.Code, status, rec.Body.String())
		}
	}
	decode := func(rec *httptest.ResponseRecorder, target any) {
		t.Helper()
		var envelope struct {
			Data json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(envelope.Data, target); err != nil {
			t.Fatal(err)
		}
	}
	enable := map[string]any{"expected_generation": 0, "enabled": true, "role": "viewer", "expires_in_seconds": 3600, "reset": false, "confirm_interactive_access": false}
	check(call("PUT", path+"/code-access", enable, nil, "https://login.pprbt.dev", ""), 401)
	check(call("PUT", path+"/code-access", enable, ownerCookies, "https://login.pprbt.dev", ""), 403)
	check(call("PUT", path+"/code-access", enable, otherCookies, "https://login.pprbt.dev", csrfCookie(t, otherCookies)), 404)
	activated := call("PUT", path+"/code-access", enable, ownerCookies, "https://login.pprbt.dev", csrfCookie(t, ownerCookies))
	check(activated, 200)
	var state usermachines.TerminalCodeAccess
	decode(activated, &state)
	if state.Code == "" || !state.Enabled {
		t.Fatal("activation lacked code")
	}
	check(call("GET", path+"/browser-session", nil, nil, "", ""), 404)
	owner := call("GET", path+"/browser-session", nil, ownerCookies, "", "")
	check(owner, 200)
	if !strings.Contains(owner.Body.String(), `"access_method":"account"`) {
		t.Fatal("owner did not take account branch")
	}
	check(call("GET", path+"/browser-session", nil, otherCookies, "", ""), 404)
	joinBody := map[string]string{"code": state.Code, "display_name": "Guest"}
	check(call("POST", path+"/code-join", joinBody, nil, "", ""), 403)
	check(call("POST", path+"/code-join", joinBody, nil, "https://other.test", ""), 403)
	// Reproduce the terminal closing between admission commit and metadata
	// lookup. The failed response must release its undelivered guest grant.
	functionName, triggerName := "code_http_close_"+suffix, "code_http_close_trigger_"+suffix
	exec(fmt.Sprintf(`CREATE FUNCTION paperboat.%s() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
      UPDATE paperboat.user_machine_terminal_sessions SET desired_state='closed',runtime_state='closed' WHERE id=NEW.terminal_session_id;
      RETURN NEW;
    END $$`, functionName))
	defer store.SQL().ExecContext(context.Background(), "DROP FUNCTION IF EXISTS paperboat."+functionName+"() CASCADE")
	exec(fmt.Sprintf(`CREATE TRIGGER %s AFTER INSERT ON paperboat.terminal_guest_sessions
      FOR EACH ROW WHEN (NEW.terminal_session_id='%s') EXECUTE FUNCTION paperboat.%s()`, triggerName, terminal, functionName))
	failedJoin := call("POST", path+"/code-join", joinBody, nil, "https://login.pprbt.dev", "")
	check(failedJoin, 404)
	if len(failedJoin.Result().Cookies()) != 0 {
		t.Fatal("failed join transferred a guest grant")
	}
	var abandoned int
	if err = store.SQL().QueryRowContext(ctx, `SELECT count(*) FROM terminal_guest_sessions WHERE terminal_session_id=$1`, terminal).Scan(&abandoned); err != nil {
		t.Fatal(err)
	}
	if abandoned != 0 {
		t.Fatalf("failed metadata lookup abandoned %d guest sessions", abandoned)
	}
	exec("DROP TRIGGER " + triggerName + " ON paperboat.terminal_guest_sessions")
	exec("DROP FUNCTION paperboat." + functionName + "()")
	exec(`UPDATE user_machine_terminal_sessions SET desired_state='open',runtime_state='running' WHERE id=$1`, terminal)
	joined := call("POST", path+"/code-join", joinBody, nil, "https://login.pprbt.dev", "")
	check(joined, 200)
	cookies := joined.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("guest cookies %d", len(cookies))
	}
	cookie := cookies[0]
	if !cookie.Secure || !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode || cookie.Path != "/" || !strings.HasPrefix(cookie.Name, "__Host-") {
		t.Fatal("guest cookie missing protected attributes")
	}
	// A cancelled request does not own the cleanup deadline, and cleanup is
	// exact-resource scoped: it cannot discard a guest from another terminal.
	abandonedGuest, err := machines.JoinTerminalWithCode(ctx, terminal, state.Code, "Cancelled join")
	if err != nil {
		t.Fatal(err)
	}
	cancelled, cancelRequest := context.WithCancel(ctx)
	cancelRequest()
	cleanupCtx, cancelCleanup := context.WithTimeout(context.WithoutCancel(cancelled), 2*time.Second)
	defer cancelCleanup()
	if err = machines.DiscardTerminalGuest(cleanupCtx, second, abandonedGuest.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = machines.AuthenticateTerminalGuest(ctx, terminal, abandonedGuest.Token); err != nil {
		t.Fatalf("wrong-terminal cleanup removed guest: %v", err)
	}
	if err = machines.DiscardTerminalGuest(cleanupCtx, terminal, abandonedGuest.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = machines.AuthenticateTerminalGuest(ctx, terminal, abandonedGuest.Token); err != usermachines.ErrTerminalCodeInvalid {
		t.Fatalf("cancelled admission not discarded: %v", err)
	}
	check(call("GET", path+"/browser-session", nil, cookies, "", ""), 200)
	check(call("GET", "/v1/me", nil, cookies, "", ""), 401)
	check(call("GET", "/v1/terminal-sessions", nil, cookies, "", ""), 401)
	check(call("GET", "/v1/terminal-sessions/"+second+"/browser-session", nil, cookies, "", ""), 404)
	check(call("GET", path+"/browser-session", nil, append(append([]*http.Cookie{}, cookies...), cookies...), "", ""), 404)
	// Signed-in users without an account grant can enter via the separate code grant.
	joinedOther := call("POST", path+"/code-join", joinBody, otherCookies, "https://login.pprbt.dev", "")
	check(joinedOther, 200)
	combined := append(append([]*http.Cookie{}, otherCookies...), joinedOther.Result().Cookies()...)
	allowedByCode := call("GET", path+"/browser-session", nil, combined, "", "")
	check(allowedByCode, 200)
	if !strings.Contains(allowedByCode.Body.String(), `"access_method":"code"`) {
		t.Fatal("unlisted account did not use guest branch")
	}
	ticketBody := map[string]string{"browser_public_key_sha256": strings.Repeat("A", 43)}
	check(call("POST", path+"/browser-ticket", ticketBody, cookies, "https://login.pprbt.dev", ""), 401)
	check(call("POST", path+"/guest-browser-ticket", ticketBody, cookies, "https://other.test", ""), 403)
	check(call("POST", path+"/guest-browser-ticket", ticketBody, nil, "https://login.pprbt.dev", ""), 404)
	stop := map[string]any{"expected_generation": state.Generation, "enabled": false, "role": "viewer", "expires_in_seconds": 3600, "reset": false, "confirm_interactive_access": false}
	check(call("PUT", path+"/code-access", stop, ownerCookies, "https://login.pprbt.dev", csrfCookie(t, ownerCookies)), 200)
	check(call("GET", path+"/browser-session", nil, cookies, "", ""), 404)
	check(call("POST", path+"/guest-browser-ticket", ticketBody, cookies, "https://login.pprbt.dev", ""), 404)
	check(call("POST", path+"/code-join", joinBody, nil, "https://login.pprbt.dev", ""), 404)
	check(call("GET", path+"/browser-session", nil, ownerCookies, "", ""), 200)
}

func TestTerminalBrowserTicketKeepsPureRejectionsAndReportsMixedFailures(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cause  error
		status int
	}{
		{"invalid key", browserterminal.ErrBrowserKeyInvalid, http.StatusBadRequest},
		{"denied", browserterminal.ErrForbidden, http.StatusNotFound},
		{"invalid ticket", browserterminal.ErrTicketInvalid, http.StatusNotFound},
		{"invalid key with IO", errors.Join(browserterminal.ErrBrowserKeyInvalid, syscall.EIO), http.StatusServiceUnavailable},
		{"denied with IO", errors.Join(browserterminal.ErrForbidden, syscall.EIO), http.StatusServiceUnavailable},
		{"invalid ticket with IO", errors.Join(browserterminal.ErrTicketInvalid, syscall.EIO), http.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request, logs, reference := diagnosticRequest("/v1/terminal-sessions/private-session/browser-ticket")
			response := httptest.NewRecorder()
			writeTerminalBrowserTicket(response, request, nil, browserterminal.Ticket{}, "", browserterminal.Decision{}, tc.cause)
			if response.Code != tc.status {
				t.Fatalf("status=%d want%d", response.Code, tc.status)
			}
			if tc.status < 500 {
				if logs.Len() != 0 {
					t.Fatal("expected rejection produced operational log")
				}
				return
			}
			var body ErrorResponse
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body.Error.Code != "terminal_unavailable" || body.Error.SupportReference != reference {
				t.Fatal("terminal response lost safe fault reference")
			}
			var event map[string]any
			if err := json.Unmarshal(logs.Bytes(), &event); err != nil {
				t.Fatalf("want one operational event: %v", err)
			}
			if event["errno"] != float64(syscall.EIO) || event["support_reference"] != reference {
				t.Fatal("terminal fault lost original IO evidence")
			}
			if strings.Contains(response.Body.String()+logs.String(), "private-session") {
				t.Fatal("private terminal identity leaked")
			}
		})
	}
}
