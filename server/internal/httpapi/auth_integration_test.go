package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/pinksaucepasta/paperboat/server/internal/accountprefs"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/audit"
	"github.com/pinksaucepasta/paperboat/server/internal/auth"
	"github.com/pinksaucepasta/paperboat/server/internal/billing"
	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/environment"
	"github.com/pinksaucepasta/paperboat/server/internal/testauth"
	"github.com/pinksaucepasta/paperboat/server/internal/usermachines"
)

func TestAuthLoginMeCSRFLogoutAndAudit(t *testing.T) {
	store, router := newAuthIntegrationRouter(t)
	state := authState(t, router)
	login := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/auth/workos/callback", strings.NewReader(`{"code":"workos_test:login@example.com:Login User","state":"`+state.Value+`"}`))
	req.AddCookie(state.Cookie)
	router.ServeHTTP(login, req)
	if login.Code != http.StatusOK {
		t.Fatalf("login status = %d, body = %s", login.Code, login.Body.String())
	}
	if strings.Contains(login.Body.String(), `"return_to"`) {
		t.Fatal("callback unexpectedly owns console navigation")
	}
	cookies := login.Result().Cookies()

	me := httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/v1/me", nil)
	addCookies(req, cookies)
	router.ServeHTTP(me, req)
	if me.Code != http.StatusOK {
		t.Fatalf("me status = %d, body = %s", me.Code, me.Body.String())
	}
	if !strings.Contains(me.Body.String(), "login@example.com") {
		t.Fatalf("me body missing user email: %s", me.Body.String())
	}

	csrf := httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/v1/auth/csrf", nil)
	addCookies(req, cookies)
	router.ServeHTTP(csrf, req)
	if csrf.Code != http.StatusOK {
		t.Fatalf("csrf status = %d, body = %s", csrf.Code, csrf.Body.String())
	}
	csrfToken := csrfCookie(t, cookies)

	logout := httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/v1/auth/logout", nil)
	addCookies(req, cookies)
	req.Header.Set(auth.CSRFHeaderName, csrfToken)
	router.ServeHTTP(logout, req)
	if logout.Code != http.StatusOK {
		t.Fatalf("logout status = %d, body = %s", logout.Code, logout.Body.String())
	}

	var auditCount int
	if err := store.SQL().QueryRowContext(context.Background(), `
SELECT count(*) FROM paperboat.audit_events WHERE event_type IN ('auth.login', 'auth.logout')`).Scan(&auditCount); err != nil {
		t.Fatal(err)
	}
	if auditCount < 2 {
		t.Fatalf("audit event count = %d, want at least 2", auditCount)
	}
}

func TestTerminalSessionMutationsRequireBrowserCSRF(t *testing.T) {
	_, router := newAuthIntegrationRouter(t)
	cookies := loginCookies(t, router, "terminal_csrf:terminal-csrf@example.com:Terminal CSRF")
	csrfToken := csrfCookie(t, cookies)
	for _, tc := range []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodPost, "/v1/machines/missing/terminal-sessions", `{"name":"work"}`},
		{http.MethodPatch, "/v1/machines/missing/terminal-sessions/missing", `{"name":"work"}`},
		{http.MethodPost, "/v1/machines/missing/terminal-sessions/missing/close", "{}"},
		{http.MethodDelete, "/v1/machines/missing/terminal-sessions/missing", ""},
	} {
		without := httptest.NewRecorder()
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		addCookies(req, cookies)
		router.ServeHTTP(without, req)
		if without.Code != http.StatusForbidden || !strings.Contains(without.Body.String(), `"csrf_failed"`) {
			t.Fatalf("%s %s without CSRF: status=%d body=%s", tc.method, tc.path, without.Code, without.Body.String())
		}
		with := httptest.NewRecorder()
		req = httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		addCookies(req, cookies)
		req.Header.Set(auth.CSRFHeaderName, csrfToken)
		if tc.method == http.MethodPost && tc.path == "/v1/machines/missing/terminal-sessions" {
			req.Header.Set("Idempotency-Key", "terminal-csrf-test-operation")
		}
		router.ServeHTTP(with, req)
		if with.Code != http.StatusNotFound {
			t.Fatalf("%s %s with CSRF: status=%d body=%s", tc.method, tc.path, with.Code, with.Body.String())
		}
	}
}

func TestCSRFEndpointBootstrapsMissingCSRFCookie(t *testing.T) {
	_, router := newAuthIntegrationRouter(t)
	cookies := loginCookies(t, router, "workos_cli_csrf:cli-csrf@example.com:CLI CSRF")
	var sessionCookie *http.Cookie
	for _, cookie := range cookies {
		if cookie.Name == auth.SessionCookieName {
			sessionCookie = cookie
			break
		}
	}
	if sessionCookie == nil {
		t.Fatal("missing session cookie")
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/auth/csrf", nil)
	req.AddCookie(sessionCookie)
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("csrf status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var gotCSRF, gotSession bool
	for _, cookie := range rec.Result().Cookies() {
		if cookie.Name == auth.CSRFCookieName && cookie.Value != "" {
			gotCSRF = true
		}
		if cookie.Name == auth.SessionCookieName {
			gotSession = true
		}
	}
	if !gotCSRF || gotSession {
		t.Fatalf("expected csrf cookie without session rotation, got %#v", rec.Result().Cookies())
	}
	if !strings.Contains(rec.Body.String(), `"csrf_token"`) {
		t.Fatalf("csrf response missing token: %s", rec.Body.String())
	}
}

func TestWorkOSCallbackRejectsMissingState(t *testing.T) {
	_, router := newAuthIntegrationRouter(t)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/auth/workos/callback", strings.NewReader(`{"code":"workos_test:login@example.com:Login User"}`)))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("callback without state status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func TestCookieWriteRequiresCSRFButPersonalFreeNeedsNoSubscription(t *testing.T) {
	store, router := newAuthIntegrationRouter(t)
	cookies := loginCookies(t, router, "workos_free:free@example.com:Free User")
	accounts := auth.NewService(store, audit.NewWriter(store), testauth.FakeWorkOSVerifier{}, []string{"test-session-key"}, true, "https://login.example.test")
	protected := authTestOriginHandler{next: requireAuth(accounts, requireCSRF(accounts, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })))}
	for _, withCSRF := range []bool{false, true} {
		r := httptest.NewRequest(http.MethodPost, "/personal-action", strings.NewReader(`{}`))
		addCookies(r, cookies)
		if withCSRF {
			r.Header.Set(auth.CSRFHeaderName, csrfCookie(t, cookies))
		}
		w := httptest.NewRecorder()
		protected.ServeHTTP(w, r)
		want := http.StatusForbidden
		if withCSRF {
			want = http.StatusNoContent
		}
		if w.Code != want {
			t.Fatalf("csrf=%v status=%d want=%d", withCSRF, w.Code, want)
		}
	}
}

func TestSafeIntegrationTestDSNRequiresTestLikeDatabaseName(t *testing.T) {
	for _, dsn := range []string{"postgres://user:pass@localhost/paperboat_test?sslmode=disable"} {
		if !safeIntegrationTestDSN(dsn) {
			t.Fatalf("dsn %q should be considered safe for integration tests", dsn)
		}
	}
	for _, dsn := range []string{
		"postgres://user:pass@localhost/paperboat_dev?sslmode=disable",
		"postgres://user:pass@localhost/paperboat_local?sslmode=disable",
		"postgres://user:pass@localhost/paperboat_test_backup?sslmode=disable",
		"postgres://user:pass@localhost/paperboat_prod?sslmode=disable",
		"postgres://user:pass@localhost/postgres?sslmode=disable",
		"not a dsn",
	} {
		if safeIntegrationTestDSN(dsn) {
			t.Fatalf("dsn %q should not be considered safe for integration tests", dsn)
		}
	}
	if !sameDatabaseDSN(
		"postgres://test:secret@db.example/paperboat_test?sslmode=require",
		"postgres://production:different@db.example/paperboat_test?sslmode=disable",
	) {
		t.Fatal("same database host and name must be rejected even when credentials differ")
	}
}

func newAuthIntegrationRouter(t *testing.T) (*db.DB, http.Handler) {
	t.Helper()
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("set PAPERBOAT_TEST_DATABASE_DSN to run auth integration tests")
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
	auditWriter := audit.NewWriter(store)
	service := auth.NewService(store, auditWriter, testauth.FakeWorkOSVerifier{}, []string{"test-session-key"}, true, "https://login.example.test")
	machineService := auth.NewDeviceService(store, auditWriter, config.Default().CLIAuth, []string{"test-machine-hash-key"})
	machineService.ConfigureAuthorizationEncryptionKey("test-enrollment-key")
	billingService := billing.NewWorkspaceService(store, nil, "test-billing-key")
	if err := billingService.InitializeAllowancePolicy(context.Background(), nil, nil); err != nil {
		t.Fatal(err)
	}
	userMachineService := usermachines.New(store, auditWriter, usermachines.Policy{PairingLifetime: 10 * time.Minute}, billingService)
	userMachineService.ConfigureInstallationEncryption("test-enrollment-key")
	machineService.SetEnroller(usermachines.DeviceEnroller{Service: userMachineService})
	cfg := config.Default()
	return store, authTestOriginHandler{next: NewRouter(Options{
		Config: cfg,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		ReadinessChecker: readinessFunc(func(context.Context) error {
			return nil
		}),
		Auth:                 service,
		DeviceAuth:           machineService,
		AccountPreferences:   accountprefs.New(store),
		WorkspaceBilling:     billingService,
		Machines:             userMachineService,
		EnvironmentVariables: environment.NewService(store, nil, nil, "http://127.0.0.1:8080"),
	})}
}

type authTestOriginHandler struct{ next http.Handler }

func (h authTestOriginHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if unsafeMethod(r.Method) && r.Header.Get("Origin") == "" {
		r.Header.Set("Origin", "https://login.example.test")
	}
	h.next.ServeHTTP(w, r)
}

func resetIntegrationTables(t *testing.T, store *db.DB) {
	t.Helper()
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if !safeIntegrationTestDSN(dsn) || sameDatabaseDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")) {
		t.Fatalf("refusing to truncate paperboat schema: PAPERBOAT_TEST_DATABASE_DSN must name a dedicated *_test database and must not match PAPERBOAT_DATABASE_DSN")
	}
	if _, err := store.SQL().ExecContext(context.Background(), `
DO $$
DECLARE
	tables text;
BEGIN
	SELECT string_agg(format('%I.%I', schemaname, tablename), ', ')
	INTO tables
	FROM pg_tables
	WHERE schemaname = 'paperboat'
	  AND tablename NOT IN ('schema_migrations', 'goose_db_version');

	IF tables IS NOT NULL THEN
		EXECUTE 'TRUNCATE TABLE ' || tables || ' CASCADE';
	END IF;
END $$;`); err != nil {
		t.Fatal(err)
	}
	// Rows the migrations seed are part of the schema, not test data.
	if _, err := store.SQL().ExecContext(context.Background(), `INSERT INTO paperboat.client_update_policy(singleton) VALUES(true) ON CONFLICT DO NOTHING`); err != nil {
		t.Fatal(err)
	}
}

func safeIntegrationTestDSN(dsn string) bool {
	u, err := url.Parse(dsn)
	if err != nil {
		return false
	}
	database := strings.Trim(strings.TrimSpace(u.Path), "/")
	if database == "" {
		return false
	}
	name := strings.ToLower(database)
	return strings.HasSuffix(name, "_test")
}

func sameDatabaseDSN(left, right string) bool {
	if strings.TrimSpace(right) == "" {
		return false
	}
	l, lerr := url.Parse(left)
	r, rerr := url.Parse(right)
	return lerr == nil && rerr == nil && strings.EqualFold(l.Host, r.Host) && strings.EqualFold(strings.Trim(l.Path, "/"), strings.Trim(r.Path, "/"))
}

func loginCookies(t *testing.T, router http.Handler, code string) []*http.Cookie {
	t.Helper()
	state := authState(t, router)
	body, _ := json.Marshal(map[string]string{"code": code, "state": state.Value})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/auth/workos/callback", bytes.NewReader(body))
	req.AddCookie(state.Cookie)
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("login status = %d, body = %s", rec.Code, rec.Body.String())
	}
	return rec.Result().Cookies()
}

type issuedState struct {
	Value  string
	Cookie *http.Cookie
}

func authState(t *testing.T, router http.Handler) issuedState {
	t.Helper()
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/auth/workos/state", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("state status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var payload struct {
		Data struct {
			State string `json:"state"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	for _, cookie := range rec.Result().Cookies() {
		if cookie.Name == auth.OAuthStateCookieName {
			return issuedState{Value: payload.Data.State, Cookie: cookie}
		}
	}
	t.Fatal("missing oauth state cookie")
	return issuedState{}
}

func addCookies(req *http.Request, cookies []*http.Cookie) {
	for _, cookie := range cookies {
		req.AddCookie(cookie)
	}
}

func csrfCookie(t *testing.T, cookies []*http.Cookie) string {
	t.Helper()
	for _, cookie := range cookies {
		if cookie.Name == auth.CSRFCookieName {
			return cookie.Value
		}
	}
	t.Fatal("missing csrf cookie")
	return ""
}

func userIDByEmail(t *testing.T, store *db.DB, email string) string {
	t.Helper()
	var id string
	if err := store.SQL().QueryRowContext(context.Background(), `SELECT id FROM paperboat.users WHERE primary_email = $1 ORDER BY created_at DESC LIMIT 1`, email).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestConsoleTerminalCreateIsRefusedAtTheBrowserTerminalLimitPostgres(t *testing.T) {
	store, router := newAuthIntegrationRouter(t)
	cookies := loginCookies(t, router, "terminal_limit:terminal-limit@example.com:Terminal Limit")
	csrfToken := csrfCookie(t, cookies)
	userID := userIDByEmail(t, store, "terminal-limit@example.com")
	ctx := context.Background()
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.user_machines(id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state,online,installation_generation)
 VALUES('machine_limit',$1,'machine_limit','limit-machine','linux','amd64','/workspace','online','occupied',true,1)`, userID); err != nil {
		t.Fatal(err)
	}
	// Free plan: 10 open browser terminals, all taken.
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.native_usage_authorities(access_session_id,machine_id,installation_generation,workspace_id,resource_kind,resource_id,resource_name,machine_name,capabilities,admitted_at,expires_at,retain_until,edge_node_id)
 SELECT 'open_'||i,'machine_limit',1,'personal:'||$1,'terminal','terminal_'||i,'open','limit-machine',ARRAY['terminal'],now(),now()+interval '10 minutes',now()+interval '11 minutes','node_test' FROM generate_series(1,10) i`, userID); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/machines/machine_limit/terminal-sessions", strings.NewReader(`{"name":"work"}`))
	addCookies(req, cookies)
	req.Header.Set(auth.CSRFHeaderName, csrfToken)
	req.Header.Set("Idempotency-Key", "terminal-limit-test-operation")
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"browser_terminal_limit_reached"`) {
		t.Fatalf("create at the limit: status=%d body=%s", rec.Code, rec.Body.String())
	}
	var sessions int
	if err := store.SQL().QueryRowContext(ctx, `SELECT count(*) FROM paperboat.user_machine_terminal_sessions WHERE user_machine_id='machine_limit'`).Scan(&sessions); err != nil {
		t.Fatal(err)
	}
	if sessions != 0 {
		t.Fatalf("a refused create left %d terminal sessions; no shell may start", sessions)
	}
}

func TestAccountPreferencesFileTransferModePostgres(t *testing.T) {
	_, router := newAuthIntegrationRouter(t)
	cookies := loginCookies(t, router, "prefs:prefs@example.com:Prefs")
	csrfToken := csrfCookie(t, cookies)
	call := func(method, body string) (int, string) {
		t.Helper()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(method, "/v1/me/preferences", strings.NewReader(body))
		addCookies(req, cookies)
		if method == http.MethodPatch {
			req.Header.Set(auth.CSRFHeaderName, csrfToken)
		}
		router.ServeHTTP(rec, req)
		return rec.Code, rec.Body.String()
	}
	if code, body := call(http.MethodGet, ""); code != http.StatusOK || !strings.Contains(body, `"file_transfer_mode":"automatic"`) {
		t.Fatalf("default preference: %d %s", code, body)
	}
	if code, body := call(http.MethodPatch, `{"file_transfer_mode":"unlimited"}`); code != http.StatusOK || !strings.Contains(body, `"file_transfer_mode":"unlimited"`) {
		t.Fatalf("set unlimited: %d %s", code, body)
	}
	if code, body := call(http.MethodGet, ""); code != http.StatusOK || !strings.Contains(body, `"file_transfer_mode":"unlimited"`) {
		t.Fatalf("read back: %d %s", code, body)
	}
	for _, bad := range []string{`{"file_transfer_mode":"fast"}`, `{"theme":"dark"}`, `not json`} {
		if code, body := call(http.MethodPatch, bad); code != http.StatusBadRequest {
			t.Fatalf("patch %s: %d %s", bad, code, body)
		}
	}
	if code, body := call(http.MethodPatch, `{}`); code != http.StatusOK || !strings.Contains(body, `"file_transfer_mode":"unlimited"`) {
		t.Fatalf("empty patch must keep the value: %d %s", code, body)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPatch, "/v1/me/preferences", strings.NewReader(`{"file_transfer_mode":"automatic"}`))
	addCookies(req, cookies)
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cookie patch without CSRF: %d", rec.Code)
	}
}
