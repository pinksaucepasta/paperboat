package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/server/internal/audit"
	"github.com/pinksaucepasta/paperboat/server/internal/auth"
	"github.com/pinksaucepasta/paperboat/server/internal/billing"
	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	pbgithub "github.com/pinksaucepasta/paperboat/server/internal/github"
	"github.com/pinksaucepasta/paperboat/server/internal/testauth"
)

func TestGitHubOAuthProvisionAndSecretRedaction(t *testing.T) {
	store, router, fake := newGitHubIntegrationRouter(t, []string{"repo"})
	cookies := loginCookies(t, router, "workos_github:github@example.com:GitHub User")
	csrf := csrfCookie(t, cookies)

	start := httptest.NewRecorder()
	body, _ := json.Marshal(map[string]string{"redirect_uri": "http://localhost:3000/github/callback"})
	req := httptest.NewRequest(http.MethodPost, "/v1/github/oauth/start", bytes.NewReader(body))
	addCookies(req, cookies)
	req.Header.Set(auth.CSRFHeaderName, csrf)
	router.ServeHTTP(start, req)
	if start.Code != http.StatusOK {
		t.Fatalf("oauth start status = %d, body = %s", start.Code, start.Body.String())
	}
	state := jsonField(t, start.Body.Bytes(), "state")
	stateCookie := cookieByName(t, start.Result().Cookies(), auth.OAuthStateCookieName)

	callback := httptest.NewRecorder()
	body, _ = json.Marshal(map[string]string{"code": "github-code", "redirect_uri": "http://localhost:3000/github/callback", "state": state})
	req = httptest.NewRequest(http.MethodPost, "/v1/github/oauth/callback", bytes.NewReader(body))
	addCookies(req, replaceCookie(cookies, stateCookie))
	req.Header.Set(auth.CSRFHeaderName, csrf)
	router.ServeHTTP(callback, req)
	if callback.Code != http.StatusOK {
		t.Fatalf("oauth callback status = %d, body = %s", callback.Code, callback.Body.String())
	}
	if strings.Contains(callback.Body.String(), "fake-gh-token") {
		t.Fatal("oauth callback leaked token")
	}

	provision := httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/v1/github/config-repositories/provision", nil)
	addCookies(req, cookies)
	req.Header.Set(auth.CSRFHeaderName, csrf)
	req.Header.Set("Idempotency-Key", "github-provision-test")
	router.ServeHTTP(provision, req)
	if provision.Code != http.StatusOK {
		t.Fatalf("provision status = %d, body = %s", provision.Code, provision.Body.String())
	}
	if !strings.Contains(provision.Body.String(), `"default_branch"`) || strings.Contains(provision.Body.String(), `"DefaultBranch"`) {
		t.Fatalf("provision response did not use snake_case fields: %s", provision.Body.String())
	}
	if fake.Created != 1 {
		t.Fatalf("created repos = %d, want 1", fake.Created)
	}
	if !strings.Contains(strings.Join(fake.PutFiles, "\n"), ".paperboat/preview-url-skill.md") {
		t.Fatalf("preview URL skill fixture was not initialized: %#v", fake.PutFiles)
	}

	retry := httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/v1/github/config-repositories/provision", nil)
	addCookies(req, cookies)
	req.Header.Set(auth.CSRFHeaderName, csrf)
	req.Header.Set("Idempotency-Key", "github-provision-test")
	router.ServeHTTP(retry, req)
	if retry.Code != http.StatusOK {
		t.Fatalf("retry status = %d, body = %s", retry.Code, retry.Body.String())
	}
	if fake.Created != 1 {
		t.Fatalf("idempotent retry created duplicate repo, created = %d", fake.Created)
	}
	if len(fake.PutFiles) != 3 {
		t.Fatalf("idempotent retry rewrote initialized files, put files = %#v", fake.PutFiles)
	}

	var leaked int
	if err := store.SQL().QueryRowContext(context.Background(), `
SELECT count(*) FROM paperboat.audit_events WHERE metadata::text LIKE '%fake-gh-token%'`).Scan(&leaked); err != nil {
		t.Fatal(err)
	}
	if leaked != 0 {
		t.Fatal("audit metadata leaked GitHub token")
	}
}

func TestGitHubOAuthRejectsMissingRequiredScopes(t *testing.T) {
	_, router, _ := newGitHubIntegrationRouter(t, []string{"read:user"})
	cookies := loginCookies(t, router, "workos_github_scope:github-scope@example.com:GitHub Scope")
	csrf := csrfCookie(t, cookies)

	start := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/github/oauth/start", strings.NewReader(`{"redirect_uri":"http://localhost:3000/github/callback"}`))
	addCookies(req, cookies)
	req.Header.Set(auth.CSRFHeaderName, csrf)
	router.ServeHTTP(start, req)
	state := jsonField(t, start.Body.Bytes(), "state")
	stateCookie := cookieByName(t, start.Result().Cookies(), auth.OAuthStateCookieName)

	callback := httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/v1/github/oauth/callback", strings.NewReader(`{"code":"github-code","redirect_uri":"http://localhost:3000/github/callback","state":"`+state+`"}`))
	addCookies(req, replaceCookie(cookies, stateCookie))
	req.Header.Set(auth.CSRFHeaderName, csrf)
	router.ServeHTTP(callback, req)
	if callback.Code != http.StatusForbidden {
		t.Fatalf("callback status = %d, body = %s", callback.Code, callback.Body.String())
	}
	if !strings.Contains(callback.Body.String(), "github_scope_denied") {
		t.Fatalf("expected github_scope_denied, body = %s", callback.Body.String())
	}
}

func TestGitHubOAuthRejectsIdentityLinkedToAnotherUser(t *testing.T) {
	_, router, _ := newGitHubIntegrationRouter(t, []string{"repo"})
	cookiesA := loginCookies(t, router, "workos_github_a:github-a@example.com:GitHub A")
	cookiesB := loginCookies(t, router, "workos_github_b:github-b@example.com:GitHub B")
	connectGitHub(t, router, cookiesA, csrfCookie(t, cookiesA))

	start := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/github/oauth/start", strings.NewReader(`{"redirect_uri":"http://localhost:3000/github/callback"}`))
	addCookies(req, cookiesB)
	req.Header.Set(auth.CSRFHeaderName, csrfCookie(t, cookiesB))
	router.ServeHTTP(start, req)
	if start.Code != http.StatusOK {
		t.Fatalf("oauth start status = %d, body = %s", start.Code, start.Body.String())
	}
	state := jsonField(t, start.Body.Bytes(), "state")
	stateCookie := cookieByName(t, start.Result().Cookies(), auth.OAuthStateCookieName)

	callback := httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/v1/github/oauth/callback", strings.NewReader(`{"code":"github-code","redirect_uri":"http://localhost:3000/github/callback","state":"`+state+`"}`))
	addCookies(req, replaceCookie(cookiesB, stateCookie))
	req.Header.Set(auth.CSRFHeaderName, csrfCookie(t, cookiesB))
	router.ServeHTTP(callback, req)
	if callback.Code != http.StatusConflict {
		t.Fatalf("callback status = %d, body = %s", callback.Code, callback.Body.String())
	}
	if !strings.Contains(callback.Body.String(), "github_identity_conflict") {
		t.Fatalf("expected github_identity_conflict, body = %s", callback.Body.String())
	}
}

func TestGitHubOAuthStartDefaultsToPublicServerCallback(t *testing.T) {
	_, router, _ := newGitHubIntegrationRouter(t, []string{"repo"})
	cookies := loginCookies(t, router, "workos_github_ngrok:github-ngrok@example.com:GitHub Ngrok")
	req := httptest.NewRequest(http.MethodPost, "/v1/github/oauth/start", strings.NewReader(`{}`))
	addCookies(req, cookies)
	req.Header.Set(auth.CSRFHeaderName, csrfCookie(t, cookies))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("oauth start status = %d, body = %s", rec.Code, rec.Body.String())
	}
	authorizationURL := jsonField(t, rec.Body.Bytes(), "authorization_url")
	if !strings.Contains(authorizationURL, "redirect_uri=https%3A%2F%2Funified-camel-humorous.ngrok-free.app%2Fv1%2Fgithub%2Foauth%2Fcallback") {
		t.Fatalf("authorization url did not use ngrok server callback: %s", authorizationURL)
	}
}

func TestGitHubBrowserCallbackCompletesOAuth(t *testing.T) {
	_, router, _ := newGitHubIntegrationRouter(t, []string{"repo"})
	cookies := loginCookies(t, router, "workos_github_browser:github-browser@example.com:GitHub Browser")
	csrf := csrfCookie(t, cookies)
	start := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/github/oauth/start", strings.NewReader(`{"return_to":"/config"}`))
	addCookies(req, cookies)
	req.Header.Set(auth.CSRFHeaderName, csrf)
	router.ServeHTTP(start, req)
	state := jsonField(t, start.Body.Bytes(), "state")
	stateCookie := cookieByName(t, start.Result().Cookies(), auth.OAuthStateCookieName)
	returnTo := cookieByName(t, start.Result().Cookies(), githubReturnToCookie)

	// GitHub returns the browser to the API origin, which has no console session.
	bounce := httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/v1/github/oauth/callback?code=github-code&state="+state, nil)
	router.ServeHTTP(bounce, req)
	if bounce.Code != http.StatusSeeOther || !strings.HasSuffix(bounce.Header().Get("Location"), "/github/callback?code=github-code&state="+url.QueryEscape(state)) {
		t.Fatalf("bare callback = %d %q", bounce.Code, bounce.Header().Get("Location"))
	}
	// The console page completes it through its proxy with the session and the start cookies.
	callback := httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/v1/github/oauth/callback", strings.NewReader(`{"code":"github-code","state":"`+state+`"}`))
	addCookies(req, append(replaceCookie(cookies, stateCookie), returnTo))
	req.Header.Set(auth.CSRFHeaderName, csrf)
	router.ServeHTTP(callback, req)
	if callback.Code != http.StatusOK || jsonField(t, callback.Body.Bytes(), "login") != "paperboat-test-user" || jsonField(t, callback.Body.Bytes(), "return_to") != "/config" {
		t.Fatalf("console callback = %d %s", callback.Code, callback.Body.String())
	}
	if strings.Contains(callback.Body.String(), "fake-gh-token") {
		t.Fatal("browser callback leaked token")
	}
}

func TestGitHubProvisionRequiresConnection(t *testing.T) {
	_, router, _ := newGitHubIntegrationRouter(t, []string{"repo"})
	cookies := loginCookies(t, router, "workos_github_required:github-required@example.com:GitHub Required")
	req := httptest.NewRequest(http.MethodPost, "/v1/github/config-repositories/provision", nil)
	addCookies(req, cookies)
	req.Header.Set(auth.CSRFHeaderName, csrfCookie(t, cookies))
	req.Header.Set("Idempotency-Key", "github-required-test")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "github_required") {
		t.Fatalf("expected github_required, body = %s", rec.Body.String())
	}
}

func TestGitHubProvisionRetriesAfterCreateFailure(t *testing.T) {
	_, router, fake := newGitHubIntegrationRouter(t, []string{"repo"})
	fake.CreateErr = errors.New("github unavailable")
	cookies := loginCookies(t, router, "workos_github_retry:github-retry@example.com:GitHub Retry")
	csrf := csrfCookie(t, cookies)
	connectGitHub(t, router, cookies, csrf)

	req := httptest.NewRequest(http.MethodPost, "/v1/github/config-repositories/provision", nil)
	addCookies(req, cookies)
	req.Header.Set(auth.CSRFHeaderName, csrf)
	req.Header.Set("Idempotency-Key", "github-retry-test")
	failed := httptest.NewRecorder()
	router.ServeHTTP(failed, req)
	if failed.Code != http.StatusServiceUnavailable {
		t.Fatalf("failed provision status = %d, body = %s", failed.Code, failed.Body.String())
	}

	fake.CreateErr = nil
	req = httptest.NewRequest(http.MethodPost, "/v1/github/config-repositories/provision", nil)
	addCookies(req, cookies)
	req.Header.Set(auth.CSRFHeaderName, csrf)
	req.Header.Set("Idempotency-Key", "github-retry-test")
	recovered := httptest.NewRecorder()
	router.ServeHTTP(recovered, req)
	if recovered.Code != http.StatusOK {
		t.Fatalf("recovered provision status = %d, body = %s", recovered.Code, recovered.Body.String())
	}
	if fake.Created != 1 {
		t.Fatalf("created repos = %d, want 1", fake.Created)
	}
}

// A connected account without the GitHub App is sent to install it by the same Connect GitHub, and nothing is
// listed or created there until it has.
func TestGitHubConnectWithoutAppSendsToInstall(t *testing.T) {
	_, router, fake := newGitHubIntegrationRouter(t, []string{"repo"}, func(cfg *config.Config) {
		cfg.GitHub.AppID, cfg.GitHub.AppSlug = "123", "paperboat-test"
	})
	fake.NotInstalled = true
	cookies := loginCookies(t, router, "workos_github_app:github-app@example.com:GitHub App")
	csrf := csrfCookie(t, cookies)
	serve := func(method, path, body string, jar []*http.Cookie) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		addCookies(req, jar)
		req.Header.Set(auth.CSRFHeaderName, csrf)
		req.Header.Set("Idempotency-Key", "github-app-test")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}
	// One Connect GitHub: start, GitHub returns the code and state, the console finishes.
	connect := func() (destination, state, finished string) {
		start := serve(http.MethodPost, "/v1/github/oauth/start", `{"return_to":"/config"}`, cookies)
		if start.Code != http.StatusOK {
			t.Fatalf("start = %d %s", start.Code, start.Body.String())
		}
		state = jsonField(t, start.Body.Bytes(), "state")
		jar := replaceCookie(cookies, cookieByName(t, start.Result().Cookies(), auth.OAuthStateCookieName))
		callback := serve(http.MethodPost, "/v1/github/oauth/callback", `{"code":"github-code","state":"`+state+`"}`, jar)
		if callback.Code != http.StatusOK {
			t.Fatalf("callback = %d %s", callback.Code, callback.Body.String())
		}
		return jsonField(t, start.Body.Bytes(), "authorization_url"), state, callback.Body.String()
	}

	first, _, connected := connect()
	if !strings.HasPrefix(first, "https://github.com/login/oauth/authorize?") {
		t.Fatalf("first connect went to %s", first)
	}
	if !strings.Contains(connected, `"install_required":true`) || fake.Created != 0 {
		t.Fatalf("connected without the app = %s, created %d", connected, fake.Created)
	}
	for _, refused := range []*httptest.ResponseRecorder{
		serve(http.MethodPost, "/v1/github/config-repositories/provision", "", cookies),
		serve(http.MethodGet, "/v1/github/repositories", "", cookies),
	} {
		if refused.Code != http.StatusConflict || !strings.Contains(refused.Body.String(), "github_app_not_installed") {
			t.Fatalf("without the app = %d %s", refused.Code, refused.Body.String())
		}
	}

	// Disconnect, then Connect GitHub, is the same path again: authorize, then the installation.
	if disconnected := serve(http.MethodDelete, "/v1/github/connection", "", cookies); disconnected.Code != http.StatusOK {
		t.Fatalf("disconnect = %d %s", disconnected.Code, disconnected.Body.String())
	}
	if again, _, body := connect(); !strings.HasPrefix(again, "https://github.com/login/oauth/authorize?") || !strings.Contains(body, `"install_required":true`) {
		t.Fatalf("connect after disconnect went to %s and answered %s", again, body)
	}

	second, state, _ := connect()
	if want := "https://github.com/apps/paperboat-test/installations/new?state=" + url.QueryEscape(state); second != want {
		t.Fatalf("connect again went to %s, want %s", second, want)
	}

	fake.NotInstalled = false // installed on GitHub
	_, _, installed := connect()
	if !strings.Contains(installed, `"install_required":false`) || !strings.Contains(installed, `"config_repo_provisioned":true`) || fake.Created != 1 {
		t.Fatalf("connected after installing = %s, created %d", installed, fake.Created)
	}
	if listed := serve(http.MethodGet, "/v1/github/repositories", "", cookies); listed.Code != http.StatusOK {
		t.Fatalf("repositories after installing = %d %s", listed.Code, listed.Body.String())
	}
}

func newGitHubIntegrationRouter(t *testing.T, tokenScopes []string, configure ...func(*config.Config)) (*db.DB, http.Handler, *pbgithub.FakeClient) {
	t.Helper()
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("set PAPERBOAT_TEST_DATABASE_DSN to run GitHub integration tests")
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
	// The same secure browser-auth setup as the auth tests: login needs an allowed origin.
	authService := auth.NewService(store, auditWriter, testauth.FakeWorkOSVerifier{}, []string{"test-session-key"}, true, "https://login.example.test")
	billingService := billing.NewWorkspaceService(store, nil, "test-billing-key")
	if err := billingService.InitializeAllowancePolicy(context.Background(), nil, nil); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.HTTP.PublicBaseURL = "https://unified-camel-humorous.ngrok-free.app"
	cfg.Secrets.EncryptionKey = "test-encryption-key-for-github-tests"
	cfg.Secrets.GitHubClientID, cfg.Secrets.GitHubClientSecret = "test-github-client", "test-github-secret"
	for _, change := range configure {
		change(&cfg)
	}
	fake := &pbgithub.FakeClient{
		Token: pbgithub.OAuthToken{AccessToken: "fake-gh-token", Scopes: tokenScopes},
		User:  pbgithub.GitHubUser{Login: "paperboat-test-user"},
	}
	githubService := pbgithub.NewService(store, auditWriter, fake, cfg)
	return store, authTestOriginHandler{next: NewRouter(Options{
		Config:           cfg,
		Logger:           slog.New(slog.NewTextHandler(io.Discard, nil)),
		ReadinessChecker: readinessFunc(func(context.Context) error { return nil }),
		Auth:             authService,
		WorkspaceBilling: billingService,
		GitHub:           githubService,
	})}, fake
}

func connectGitHub(t *testing.T, router http.Handler, cookies []*http.Cookie, csrf string) {
	t.Helper()
	start := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/github/oauth/start", strings.NewReader(`{"redirect_uri":"http://localhost:3000/github/callback"}`))
	addCookies(req, cookies)
	req.Header.Set(auth.CSRFHeaderName, csrf)
	router.ServeHTTP(start, req)
	if start.Code != http.StatusOK {
		t.Fatalf("oauth start status = %d, body = %s", start.Code, start.Body.String())
	}
	state := jsonField(t, start.Body.Bytes(), "state")
	stateCookie := cookieByName(t, start.Result().Cookies(), auth.OAuthStateCookieName)
	callback := httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/v1/github/oauth/callback", strings.NewReader(`{"code":"github-code","redirect_uri":"http://localhost:3000/github/callback","state":"`+state+`"}`))
	addCookies(req, replaceCookie(cookies, stateCookie))
	req.Header.Set(auth.CSRFHeaderName, csrf)
	router.ServeHTTP(callback, req)
	if callback.Code != http.StatusOK {
		t.Fatalf("oauth callback status = %d, body = %s", callback.Code, callback.Body.String())
	}
}

func jsonField(t *testing.T, body []byte, name string) string {
	t.Helper()
	var payload struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatal(err)
	}
	value, ok := payload.Data[name].(string)
	if !ok || value == "" {
		t.Fatalf("missing JSON field %q in %s", name, string(body))
	}
	return value
}

func cookieByName(t *testing.T, cookies []*http.Cookie, name string) *http.Cookie {
	t.Helper()
	for _, cookie := range cookies {
		if cookie.Name == name {
			return cookie
		}
	}
	t.Fatalf("missing cookie %q", name)
	return nil
}

func replaceCookie(cookies []*http.Cookie, replacement *http.Cookie) []*http.Cookie {
	out := make([]*http.Cookie, 0, len(cookies)+1)
	for _, cookie := range cookies {
		if cookie.Name != replacement.Name {
			out = append(out, cookie)
		}
	}
	return append(out, replacement)
}
