package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/audit"
	"github.com/pinksaucepasta/paperboat-server/internal/auth"
	"github.com/pinksaucepasta/paperboat-server/internal/billing"
	"github.com/pinksaucepasta/paperboat-server/internal/config"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
	"github.com/pinksaucepasta/paperboat-server/internal/environment"
	"github.com/pinksaucepasta/paperboat-server/internal/testauth"
	"github.com/pinksaucepasta/paperboat-server/internal/usermachines"
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
		{http.MethodPatch, "/v1/machines/missing/terminal-sessions/missing", `{"title":"work"}`},
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

func TestAdminBillingAdjustmentsRequireAdminAndWriteLedger(t *testing.T) {
	store, router := newAuthIntegrationRouter(t)
	adminCookies := loginCookies(t, router, "workos_admin:admin@example.com:Admin User")
	targetCookies := loginCookies(t, router, "workos_target:target@example.com:Target User")
	adminID := userIDByEmail(t, store, "admin@example.com")
	targetID := userIDByEmail(t, store, "target@example.com")
	if _, err := store.SQL().ExecContext(context.Background(), `UPDATE paperboat.users SET role = 'admin' WHERE id = $1`, adminID); err != nil {
		t.Fatal(err)
	}

	denied := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/users/"+targetID+"/adjust-credits", strings.NewReader(`{"amount":"5","reason":"test"}`))
	addCookies(req, targetCookies)
	req.Header.Set(auth.CSRFHeaderName, csrfCookie(t, targetCookies))
	req.Header.Set("Idempotency-Key", "admin-denied-"+targetID)
	router.ServeHTTP(denied, req)
	if denied.Code != http.StatusForbidden {
		t.Fatalf("non-admin status = %d, body = %s", denied.Code, denied.Body.String())
	}

	credits := httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/v1/admin/users/"+targetID+"/adjust-credits", strings.NewReader(`{"amount":"7.5","reason":"support correction"}`))
	addCookies(req, adminCookies)
	req.Header.Set(auth.CSRFHeaderName, csrfCookie(t, adminCookies))
	req.Header.Set("Idempotency-Key", "admin-credit-"+targetID)
	router.ServeHTTP(credits, req)
	if credits.Code != http.StatusOK {
		t.Fatalf("credit adjustment status = %d, body = %s", credits.Code, credits.Body.String())
	}

	storage := httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/v1/admin/users/"+targetID+"/adjust-storage", strings.NewReader(`{"purchased_gb":3,"reason":"support correction"}`))
	addCookies(req, adminCookies)
	req.Header.Set(auth.CSRFHeaderName, csrfCookie(t, adminCookies))
	req.Header.Set("Idempotency-Key", "admin-storage-"+targetID)
	router.ServeHTTP(storage, req)
	if storage.Code != http.StatusOK {
		t.Fatalf("storage adjustment status = %d, body = %s", storage.Code, storage.Body.String())
	}

	var balance string
	if err := store.SQL().QueryRowContext(context.Background(), `
SELECT balance::text FROM paperboat.credit_accounts WHERE user_id = $1`, targetID).Scan(&balance); err != nil {
		t.Fatal(err)
	}
	if balance != "7.500000" {
		t.Fatalf("balance = %s", balance)
	}
	var purchased int
	if err := store.SQL().QueryRowContext(context.Background(), `
SELECT purchased_gb FROM paperboat.storage_accounts WHERE user_id = $1`, targetID).Scan(&purchased); err != nil {
		t.Fatal(err)
	}
	if purchased != 3 {
		t.Fatalf("purchased_gb = %d", purchased)
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
	if err := billingService.InitializeAllowancePolicy(context.Background(), nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	userMachineService := usermachines.New(store, auditWriter, usermachines.Policy{PairingLifetime: 10 * time.Minute}, billingService)
	userMachineService.ConfigureInstallationEncryption("test-enrollment-key")
	cfg := config.Default()
	return store, authTestOriginHandler{next: NewRouter(Options{
		Config: cfg,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		ReadinessChecker: readinessFunc(func(context.Context) error {
			return nil
		}),
		Auth:                 service,
		DeviceAuth:           machineService,
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
