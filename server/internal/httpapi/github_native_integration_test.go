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

	"github.com/pinksaucepasta/paperboat/server/internal/audit"
	"github.com/pinksaucepasta/paperboat/server/internal/auth"
	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	pbgithub "github.com/pinksaucepasta/paperboat/server/internal/github"
	"github.com/pinksaucepasta/paperboat/server/internal/testauth"
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
	cfg.HTTP.BrowserLoginOrigin = "https://login.example.test"
	cfg.Secrets.GitHubClientID = "native-test-client"
	cfg.Secrets.EncryptionKey = "native-test-encryption-key"
	writer := audit.NewWriter(store)
	accounts := auth.NewService(store, writer, testauth.FakeWorkOSVerifier{}, []string{"native-github-session"}, true, "https://login.example.test")
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
		if _, e = cliAuth.Decide(ctx, authorization.UserCode, user, true, ""); e != nil {
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
	request := func(method, path, token, body string, browser []*http.Cookie, want int) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		for _, cookie := range browser {
			req.AddCookie(cookie)
		}
		if browser != nil && method != http.MethodGet {
			req.Header.Set(auth.CSRFHeaderName, csrfCookie(t, browser))
		}
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Fatalf("%s %s status=%d want=%d body=%s", method, strings.Split(path, "?")[0], rec.Code, want, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), "native-provider-secret") || strings.Contains(rec.Body.String(), "<html") {
			t.Fatal("provider secret or HTML escaped")
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
	// The console reads the link id from the query and the token from the fragment.
	capability := func(item pbgithub.NativeLink) (string, string) {
		t.Helper()
		target, e := url.Parse(item.BrowserURL)
		if e != nil || target.Scheme+"://"+target.Host != cfg.HTTP.BrowserLoginOrigin || target.Path != "/github/connect" || target.Fragment == "" {
			t.Fatalf("browser url is not the console connect page: %v", e)
		}
		return target.Query().Get("link"), `{"token":"` + target.Fragment + `"}`
	}
	decide := func(item pbgithub.NativeLink, action string, browser []*http.Cookie, want int) *httptest.ResponseRecorder {
		t.Helper()
		id, body := capability(item)
		return request("POST", "/v1/github/native-links/"+id+"/"+action, "", body, browser, want)
	}
	approve := func(item pbgithub.NativeLink) string {
		t.Helper()
		var out struct {
			Data struct {
				AuthorizationURL string `json:"authorization_url"`
			} `json:"data"`
		}
		if e := json.Unmarshal(decide(item, "approve", cookies, 200).Body.Bytes(), &out); e != nil {
			t.Fatal(e)
		}
		target, e := url.Parse(out.Data.AuthorizationURL)
		if e != nil || target.Query().Get("redirect_uri") != service.DefaultCallbackURL() {
			t.Fatal("callback changed")
		}
		return target.Query().Get("state")
	}
	callback := func(state string, want string) {
		t.Helper()
		rec := request("GET", "/v1/github/oauth/callback?code=provider-code&state="+url.QueryEscape(state), "", "", nil, http.StatusSeeOther)
		location := rec.Header().Get("Location")
		if !strings.HasPrefix(location, cfg.HTTP.BrowserLoginOrigin+"/github/done?") || !strings.Contains(location, want) {
			t.Fatalf("callback redirect = %q; want %q", location, want)
		}
	}
	request("POST", "/v1/github/native-links", "", "{}", nil, 401)
	request("POST", "/v1/github/native-links", read.AccessToken, "{}", nil, 403)
	item := start(first.AccessToken)
	path := "/v1/github/native-links/" + item.ID
	request("GET", path, second.AccessToken, "", nil, 404)
	request("GET", path, foreign.AccessToken, "", nil, 404)
	request("DELETE", path, foreign.AccessToken, "", nil, 404)
	// Review is read-only: a prefetch or reload must not consume the request.
	id, _ := capability(item)
	request("POST", "/v1/github/native-links/"+id+"/review", "", `{"token":"wrong"}`, cookies, 404)
	request("POST", "/v1/github/native-links/"+id+"/review", "", `{"token":"x"}`, nil, 401)
	for range 2 {
		var review struct {
			Data pbgithub.NativeLinkReview `json:"data"`
		}
		if e := json.Unmarshal(decide(item, "review", cookies, 200).Body.Bytes(), &review); e != nil || review.Data.AccountEmail != ownerEmail {
			t.Fatalf("review = %+v %v", review.Data, e)
		}
	}
	otherCookies := loginCookies(t, router, "workos_native_github_other_"+suffix+":"+otherEmail+":Other")
	decide(item, "review", otherCookies, 403)
	decide(item, "approve", otherCookies, 403)
	state := approve(item)
	decide(item, "approve", cookies, 409)
	decide(item, "review", cookies, 409)
	callback(state+"tampered", "code=github_link_expired")
	callback(state, "login=native-test-provider&result=connected")
	callback(state, "code=github_link_expired")
	if got := decode(request("GET", path, first.AccessToken, "", nil, 200)); got.State != "completed" || got.BrowserURL != "" {
		t.Fatal("poll completion invalid")
	}
	request("DELETE", path, first.AccessToken, "", nil, 200)
	// The console's Cancel ends the request for the CLI too.
	item = start(first.AccessToken)
	decide(item, "cancel", cookies, 200)
	decide(item, "approve", cookies, 409)
	if got := decode(request("GET", "/v1/github/native-links/"+item.ID, first.AccessToken, "", nil, 200)); got.State == "pending" {
		t.Fatal("console cancel left the request pending")
	}
	// Cancellation, expiry, restart replacement and client revocation fence approval and callback.
	item = start(first.AccessToken)
	state = approve(item)
	request("DELETE", "/v1/github/native-links/"+item.ID, first.AccessToken, "", nil, 200)
	callback(state, "code=github_link_expired")
	item = start(first.AccessToken)
	if _, err := store.SQL().Exec(`UPDATE paperboat.github_native_links SET expires_at=now()-interval '1 second' WHERE id=$1`, item.ID); err != nil {
		t.Fatal(err)
	}
	if got := decode(request("GET", "/v1/github/native-links/"+item.ID, first.AccessToken, "", nil, 200)); got.State != "expired" {
		t.Fatal("expiry not reported")
	}
	decide(item, "review", cookies, 410)
	old := start(first.AccessToken)
	item = start(first.AccessToken)
	decide(old, "review", cookies, 404)
	state = approve(item)
	if _, err := store.SQL().Exec(`UPDATE paperboat.cli_client_sessions SET state='revoked',revoked_at=now() WHERE id=$1`, first.CLIClientSessionID); err != nil {
		t.Fatal(err)
	}
	callback(state, "result=error")
	request("GET", "/v1/github/native-links/"+item.ID, first.AccessToken, "", nil, 401)
	// A revocation racing the provider exchange must prevent credential persistence.
	item = start(second.AccessToken)
	state = approve(item)
	var beforeVersion int64
	if err := store.SQL().QueryRow(`SELECT version FROM paperboat.github_oauth_tokens WHERE user_id=$1`, owner).Scan(&beforeVersion); err != nil {
		t.Fatal(err)
	}
	provider.beforeExchange = func() {
		if _, err := store.SQL().Exec(`UPDATE paperboat.cli_client_sessions SET state='revoked',revoked_at=now() WHERE id=$1`, second.CLIClientSessionID); err != nil {
			t.Fatal(err)
		}
	}
	callback(state, "result=error")
	var afterVersion int64
	if err := store.SQL().QueryRow(`SELECT version FROM paperboat.github_oauth_tokens WHERE user_id=$1`, owner).Scan(&afterVersion); err != nil || afterVersion != beforeVersion {
		t.Fatal("revoked client changed provider credential")
	}
	// A console-started state is sent to the console, which completes it with the session.
	request("GET", "/v1/github/oauth/callback?state=ordinary-browser-state&code=provider-code", "", "", nil, http.StatusSeeOther)
	// Status names the connected account; disconnect clears it.
	var status struct {
		Data pbgithub.Status `json:"data"`
	}
	if e := json.Unmarshal(request("GET", "/v1/github/status", "", "", cookies, 200).Body.Bytes(), &status); e != nil || status.Data.Login != "native-test-provider" || status.Data.AvatarURL == "" {
		t.Fatalf("status = %+v %v", status.Data, e)
	}
	request("DELETE", "/v1/github/connection", "", "", cookies, http.StatusConflict)
	if _, err := store.SQL().Exec(`UPDATE paperboat.control_config_repositories SET state='disconnected' WHERE owner_user_id=$1`, owner); err != nil {
		t.Fatal(err)
	}
	var disconnected struct {
		Data pbgithub.Status `json:"data"`
	}
	if e := json.Unmarshal(request("DELETE", "/v1/github/connection", "", "", cookies, 200).Body.Bytes(), &disconnected); e != nil || disconnected.Data.Connected || disconnected.Data.Login != "" {
		t.Fatalf("disconnect = %+v %v", disconnected.Data, e)
	}
}
