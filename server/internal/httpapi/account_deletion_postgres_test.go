package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/accountdeletion"
	"github.com/pinksaucepasta/paperboat/server/internal/audit"
	"github.com/pinksaucepasta/paperboat/server/internal/auth"
	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/testauth"
	"github.com/pinksaucepasta/paperboat/server/internal/usermachines"
)

func newAccountDeletionRouter(t *testing.T) (*db.DB, http.Handler) {
	t.Helper()
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("set PAPERBOAT_TEST_DATABASE_DSN to run account deletion integration tests")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	store, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := db.Migrate(context.Background(), store); err != nil {
		t.Fatal(err)
	}
	resetIntegrationTables(t, store)
	writer := audit.NewWriter(store)
	service := auth.NewService(store, writer, testauth.FakeWorkOSVerifier{}, []string{"test-session-key"}, true, "https://login.example.test")
	machineService := auth.NewDeviceService(store, writer, config.Default().CLIAuth, []string{"test-machine-hash-key"})
	mail := config.TransactionalMail{From: "sender@example.test", ConsoleOrigin: "https://console.example.test"}
	deletion := accountdeletion.New(store, usermachines.New(store, writer, usermachines.Policy{}, nil), mail)
	return store, authTestOriginHandler{next: NewRouter(Options{
		Config:          config.Default(),
		Logger:          slog.New(slog.NewTextHandler(io.Discard, nil)),
		Auth:            service,
		DeviceAuth:      machineService,
		AccountDeletion: deletion,
	})}
}

// accountDeletionCall sends one request; cookies make it a browser request, bearer makes it a CLI or API token request.
func accountDeletionCall(t *testing.T, handler http.Handler, method, body string, cookies []*http.Cookie, bearer string) (int, map[string]any, *httptest.ResponseRecorder) {
	t.Helper()
	path := "/v1/account"
	if method == http.MethodGet {
		path = "/v1/account/deletion-preview"
	}
	req := httptest.NewRequest(method, path, bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	addCookies(req, cookies)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if method == http.MethodDelete && len(cookies) > 0 {
		req.Header.Set(auth.CSRFHeaderName, csrfCookie(t, cookies))
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out, rec
}

func errorDetails(t *testing.T, out map[string]any) (code string, details map[string]any) {
	t.Helper()
	e, ok := out["error"].(map[string]any)
	if !ok {
		t.Fatalf("response is not an error: %v", out)
	}
	code, _ = e["code"].(string)
	details, _ = e["details"].(map[string]any)
	return code, details
}

func TestAccountDeletionHTTP(t *testing.T) {
	store, handler := newAccountDeletionRouter(t)
	cookies := loginCookies(t, handler, "subj_http_owner:owner@example.test:Owner")
	userID := userIDByEmail(t, store, "owner@example.test")
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(context.Background(), query, args...); err != nil {
			t.Fatal(err)
		}
	}
	setSignIn := func(ago time.Duration) {
		t.Helper()
		exec(`UPDATE sessions SET created_at=now()-make_interval(secs => $2) WHERE user_id=$1`, userID, ago.Seconds())
	}

	status, out, _ := accountDeletionCall(t, handler, http.MethodDelete, `{"confirm_email":"owner@example.test"}`, nil, "cli-token")
	if code, details := errorDetails(t, out); status != http.StatusForbidden || code != "account_deletion_console_only" || details["console_url"] != "https://console.example.test/settings/account" {
		t.Fatalf("CLI delete: status %d, %s %v", status, code, details)
	}
	if status, out, _ := accountDeletionCall(t, handler, http.MethodGet, "", nil, "api-token"); status != http.StatusForbidden {
		t.Fatalf("API token preview: %d %v", status, out)
	}

	status, out, _ = accountDeletionCall(t, handler, http.MethodGet, "", cookies, "")
	if status != http.StatusOK {
		t.Fatalf("preview: %d %v", status, out)
	}
	if data := out["data"].(map[string]any); data["account_email"] != "owner@example.test" || data["reauthentication_required"] != false {
		t.Fatalf("preview data: %v", data)
	}

	setSignIn(10 * time.Minute)
	if status, out, _ := accountDeletionCall(t, handler, http.MethodDelete, `{"confirm_email":"owner@example.test"}`, cookies, ""); status != http.StatusUnauthorized {
		t.Fatalf("stale sign-in: %d %v", status, out)
	} else if code, _ := errorDetails(t, out); code != "reauthentication_required" {
		t.Fatalf("stale sign-in code = %s", code)
	}

	setSignIn(time.Second)
	if status, out, _ := accountDeletionCall(t, handler, http.MethodDelete, `{"confirm_email":"someone@example.test"}`, cookies, ""); status != http.StatusBadRequest {
		t.Fatalf("wrong email: %d %v", status, out)
	} else if code, _ := errorDetails(t, out); code != "confirmation_mismatch" {
		t.Fatalf("wrong email code = %s", code)
	}

	exec(`INSERT INTO teams(team_id,owner_account,generation,display_name) VALUES('http-team',$1,1,'HTTP team')`, userID)
	exec(`INSERT INTO team_members(team_id,account_id,membership_generation,role,active) VALUES('http-team',$1,1,'admin',true)`, userID)
	exec(`INSERT INTO users(id,workos_subject,primary_email,status) VALUES('http-member','subj_http_member','member@example.test','active')`)
	exec(`INSERT INTO team_members(team_id,account_id,membership_generation,role,active) VALUES('http-team','http-member',1,'member',true)`)
	if status, out, _ := accountDeletionCall(t, handler, http.MethodDelete, `{"confirm_email":"owner@example.test"}`, cookies, ""); status != http.StatusConflict {
		t.Fatalf("owned team: %d %v", status, out)
	} else {
		code, details := errorDetails(t, out)
		teams, _ := details["teams"].([]any)
		if code != "account_owns_team" || len(teams) != 1 || teams[0].(map[string]any)["slug"] != "http-team" || teams[0].(map[string]any)["members"] != float64(2) {
			t.Fatalf("owned team refusal: %s %v", code, details)
		}
	}

	exec(`UPDATE teams SET deleted_at=now() WHERE team_id='http-team'`)
	status, out, rec := accountDeletionCall(t, handler, http.MethodDelete, `{"confirm_email":"owner@example.test"}`, cookies, "")
	if status != http.StatusNoContent {
		t.Fatalf("delete: %d %v", status, out)
	}
	if rec.Header().Get("Set-Cookie") == "" {
		t.Fatal("the session cookies were not cleared")
	}
	var active int
	if err := store.SQL().QueryRowContext(context.Background(), `SELECT count(*) FROM sessions WHERE user_id=$1 AND revoked_at IS NULL`, userID).Scan(&active); err != nil || active != 0 {
		t.Fatalf("sessions still active: %d, %v", active, err)
	}
	if status, _, _ := accountDeletionCall(t, handler, http.MethodGet, "", cookies, ""); status != http.StatusUnauthorized {
		t.Fatalf("preview after deletion: %d", status)
	}
}
