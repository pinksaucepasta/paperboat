package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
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
	"github.com/pinksaucepasta/paperboat-server/internal/config"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
	pbgithub "github.com/pinksaucepasta/paperboat-server/internal/github"
	"github.com/pinksaucepasta/paperboat-server/internal/testauth"
)

type nativeGitHubTestProvider struct {
	*pbgithub.FakeClient
	beforeExchange func()
}

func (f *nativeGitHubTestProvider) ExchangeOAuthCode(ctx context.Context, in pbgithub.OAuthExchangeInput) (pbgithub.OAuthToken, error) {
	if f.beforeExchange != nil {
		f.beforeExchange()
	}
	return f.FakeClient.ExchangeOAuthCode(ctx, in)
}

func TestGitHubNativeLinkAuthenticatedLifecycle(t *testing.T) {
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
	if err := db.Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.HTTP.PublicBaseURL = "https://api.example.test"
	cfg.Secrets.GitHubClientID = "native-test-client"
	cfg.Secrets.EncryptionKey = "native-test-encryption-key"
	writer := audit.NewWriter(store)
	accounts := auth.NewService(store, writer, testauth.FakeWorkOSVerifier{}, []string{"native-github-session"}, true, "https://login.pprbt.dev")
	cliAuth := auth.NewDeviceService(store, writer, cfg.CLIAuth, []string{"native-github-machine"})
	cliAuth.ConfigureAuthorizationEncryptionKey("test-device-recovery-key")
	provider := &nativeGitHubTestProvider{FakeClient: &pbgithub.FakeClient{Token: pbgithub.OAuthToken{AccessToken: "native-provider-secret", Scopes: []string{"repo"}}, User: pbgithub.GitHubUser{Login: "native-test-provider"}}}
	service := pbgithub.NewService(store, writer, provider, cfg)
	router := authTestOriginHandler{next: NewRouter(Options{Config: cfg, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Auth: accounts, DeviceAuth: cliAuth, GitHub: service})}
	suffix := fmt.Sprint(time.Now().UnixNano())
	ownerEmail := "native-github-" + suffix + "@example.test"
	cookies := loginCookies(t, router, "workos_native_github_"+suffix+":"+ownerEmail+":Owner")
	owner := userIDByEmail(t, store, ownerEmail)
	otherEmail := "native-github-other-" + suffix + "@example.test"
	_ = loginCookies(t, router, "workos_native_github_other_"+suffix+":"+otherEmail+":Other")
	other := userIDByEmail(t, store, otherEmail)
	grant := func(user string) auth.TokenSet {
		t.Helper()
		authorization, e := cliAuth.Authorize(ctx, auth.DeviceAuthorizationInput{ClientID: cfg.CLIAuth.ClientID, ClientLabel: "GitHub linking", DeviceType: "desktop", OS: "linux", Scopes: cfg.CLIAuth.AllowedScopes, Network: "127.0.0.1"})
		if e != nil {
			t.Fatal(e)
		}
		if _, e = cliAuth.Decide(ctx, authorization.UserCode, user, true); e != nil {
			t.Fatal(e)
		}
		token, e := cliAuth.Poll(ctx, auth.DeviceTokenInput{ClientID: cfg.CLIAuth.ClientID, DeviceCode: authorization.DeviceCode, Network: "127.0.0.1"})
		if e != nil {
			t.Fatal(e)
		}
		return token
	}
	first, second, foreign, read := grant(owner), grant(owner), grant(other), grant(owner)
	if _, err := store.SQL().Exec(`UPDATE paperboat.cli_client_sessions SET scopes=ARRAY['machines:read']::text[] WHERE id=$1`, read.CLIClientSessionID); err != nil {
		t.Fatal(err)
	}
	request := func(method, path, token, body string, cookie *http.Cookie, want int) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		if cookie != nil {
			req.AddCookie(cookie)
		}
		if method == http.MethodPost && strings.Contains(path, "/launch") {
			req.Header.Set("Origin", cfg.HTTP.PublicBaseURL)
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Fatalf("%s %s status=%d want=%d", method, strings.Split(path, "?")[0], rec.Code, want)
		}
		if strings.Contains(rec.Body.String(), "native-provider-secret") {
			t.Fatal("provider secret escaped")
		}
		return rec
	}
	decode := func(rec *httptest.ResponseRecorder) pbgithub.NativeLink {
		t.Helper()
		var out struct {
			Data pbgithub.NativeLink `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out.Data
	}
	start := func(token string) pbgithub.NativeLink {
		return decode(request("POST", "/v1/github/native-links", token, "{}", nil, 201))
	}
	launch := func(item pbgithub.NativeLink) *http.Cookie {
		t.Helper()
		rec := request("GET", item.BrowserURL, "", "", nil, 200)
		if !strings.Contains(rec.Body.String(), ownerEmail) {
			t.Fatal("account not shown")
		}
		if rec.Header().Get("Referrer-Policy") != "same-origin" {
			t.Fatal("launch capabilities could leak via referrer")
		}
		return cookieByName(t, rec.Result().Cookies(), nativeGitHubCookie)
	}
	authorize := func(item pbgithub.NativeLink, cookie *http.Cookie) string {
		t.Helper()
		rec := request("POST", "/v1/github/native-links/"+item.ID+"/launch", "", url.Values{"csrf": {cookie.Value}, "action": {"continue"}}.Encode(), cookie, 303)
		target, e := url.Parse(rec.Header().Get("Location"))
		if e != nil {
			t.Fatal(e)
		}
		if target.Query().Get("redirect_uri") != service.DefaultCallbackURL() {
			t.Fatal("callback changed")
		}
		return target.Query().Get("state")
	}
	callback := func(state string, cookie *http.Cookie, want int) {
		request("GET", "/v1/github/oauth/callback?code=provider-code&state="+url.QueryEscape(state), "", "", cookie, want)
	}
	request("POST", "/v1/github/native-links", "", "{}", nil, 401)
	request("POST", "/v1/github/native-links", read.AccessToken, "{}", nil, 403)
	item := start(first.AccessToken)
	path := "/v1/github/native-links/" + item.ID
	request("GET", path, second.AccessToken, "", nil, 404)
	request("GET", path, foreign.AccessToken, "", nil, 404)
	request("DELETE", path, foreign.AccessToken, "", nil, 404)
	browser := launch(item)
	request("GET", item.BrowserURL, "", "", nil, 400)
	request("POST", path+"/launch", "", "action=continue&csrf=wrong", browser, 403)
	state := authorize(item, browser)
	callback(state, nil, 403)
	wrong := *browser
	wrong.Value = strings.Repeat("x", 43)
	callback(state, &wrong, 400)
	callback(state+"tampered", browser, 400)
	callback(state, browser, 200)
	callback(state, browser, 400)
	if got := decode(request("GET", path, first.AccessToken, "", nil, 200)); got.State != "completed" || got.BrowserURL != "" {
		t.Fatal("poll completion invalid")
	}
	request("DELETE", path, first.AccessToken, "", nil, 200)
	// Cancellation, expiry, restart replacement and client revocation fence both browser and native operations.
	item = start(first.AccessToken)
	browser = launch(item)
	state = authorize(item, browser)
	request("DELETE", "/v1/github/native-links/"+item.ID, first.AccessToken, "", nil, 200)
	callback(state, browser, 400)
	item = start(first.AccessToken)
	if _, err := store.SQL().Exec(`UPDATE paperboat.github_native_links SET expires_at=now()-interval '1 second' WHERE id=$1`, item.ID); err != nil {
		t.Fatal(err)
	}
	if got := decode(request("GET", "/v1/github/native-links/"+item.ID, first.AccessToken, "", nil, 200)); got.State != "expired" {
		t.Fatal("expiry not reported")
	}
	request("GET", item.BrowserURL, "", "", nil, 400)
	old := start(first.AccessToken)
	item = start(first.AccessToken)
	request("GET", old.BrowserURL, "", "", nil, 400)
	browser = launch(item)
	state = authorize(item, browser)
	if _, err := store.SQL().Exec(`UPDATE paperboat.cli_client_sessions SET state='revoked',revoked_at=now() WHERE id=$1`, first.CLIClientSessionID); err != nil {
		t.Fatal(err)
	}
	callback(state, browser, 400)
	request("GET", "/v1/github/native-links/"+item.ID, first.AccessToken, "", nil, 401)
	// A revocation racing the provider exchange must prevent credential persistence.
	item = start(second.AccessToken)
	browser = launch(item)
	state = authorize(item, browser)
	var beforeVersion int64
	if err := store.SQL().QueryRow(`SELECT version FROM paperboat.github_oauth_tokens WHERE user_id=$1`, owner).Scan(&beforeVersion); err != nil {
		t.Fatal(err)
	}
	provider.beforeExchange = func() {
		if _, err := store.SQL().Exec(`UPDATE paperboat.cli_client_sessions SET state='revoked',revoked_at=now() WHERE id=$1`, second.CLIClientSessionID); err != nil {
			t.Fatal(err)
		}
	}
	callback(state, browser, 400)
	var afterVersion int64
	if err := store.SQL().QueryRow(`SELECT version FROM paperboat.github_oauth_tokens WHERE user_id=$1`, owner).Scan(&afterVersion); err != nil || afterVersion != beforeVersion {
		t.Fatal("revoked client changed provider credential")
	}
	// Browser OAuth remains session-authenticated; native state cannot bypass its dispatch.
	request("GET", "/v1/github/oauth/callback?state=ordinary-browser-state&code=provider-code", "", "", nil, 401)
	_ = cookies
}
