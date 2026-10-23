package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/auth"
	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	pbgithub "github.com/pinksaucepasta/paperboat/server/internal/github"
	"github.com/pinksaucepasta/paperboat/server/internal/supportreport"
)

func TestGitHubOAuthCallbackSeparatesRejectionAndOperationalFailure(t *testing.T) {
	const privateDetail = "PRIVATE_PROVIDER_DETAIL"
	for _, tc := range []struct {
		name        string
		exchangeErr error
		tokenScopes []string
		status      int
		code        string
		cause       string
		wantCapture bool
	}{
		{name: "provider transport outage", exchangeErr: errors.Join(errors.New(privateDetail), syscall.ECONNREFUSED), status: http.StatusServiceUnavailable, code: "provider_unavailable", cause: "connection_refused", wantCapture: true},
		{name: "invalid authorization code", exchangeErr: pbgithub.APIError{StatusCode: http.StatusBadRequest, Message: privateDetail}, status: http.StatusUnauthorized, code: "github_oauth_failed"},
		{name: "provider rejection with connection failure", exchangeErr: errors.Join(pbgithub.APIError{StatusCode: http.StatusBadRequest, Message: privateDetail}, syscall.ECONNREFUSED), status: http.StatusServiceUnavailable, code: "provider_unavailable", cause: "connection_refused", wantCapture: true},
		{name: "missing scope", tokenScopes: []string{"read:user"}, status: http.StatusForbidden, code: "github_scope_denied"},
		{name: "missing scope with operational failure", exchangeErr: errors.Join(pbgithub.ErrMissingScopes, syscall.ECONNREFUSED), status: http.StatusServiceUnavailable, code: "provider_unavailable", cause: "connection_refused", wantCapture: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &supportTestStore{}
			reports, err := supportreport.New(store, supportreport.Config{})
			if err != nil {
				t.Fatal(err)
			}
			runCtx, cancelReports := context.WithCancel(context.Background())
			reportsDone := make(chan struct{})
			go func() { _ = reports.Run(runCtx); close(reportsDone) }()
			defer func() {
				cancelReports()
				select {
				case <-reportsDone:
				case <-time.After(5 * time.Second):
					t.Error("support reporter did not drain")
				}
			}()

			cfg := config.Default()
			authService := auth.NewService(nil, nil, nil, []string{"github-callback-test-key"}, false)
			state, err := authService.NewOAuthState()
			if err != nil {
				t.Fatal(err)
			}
			fake := &pbgithub.FakeClient{ExchangeErr: tc.exchangeErr}
			if tc.tokenScopes != nil {
				fake.Token = pbgithub.OAuthToken{AccessToken: "fake-token", Scopes: tc.tokenScopes}
			}
			githubService := pbgithub.NewService(nil, nil, fake, cfg)
			var logs strings.Builder
			handler := supportMiddleware(reports, slog.New(slog.NewTextHandler(&logs, nil)), githubOAuthCallback(authService, githubService))
			req := httptest.NewRequest(http.MethodPost, "/v1/github/oauth/callback", strings.NewReader(`{"code":"oauth-code","redirect_uri":"https://api.example.test/callback","state":"`+state+`"}`))
			req.AddCookie(&http.Cookie{Name: auth.DevOAuthStateCookieName, Value: state})
			req = req.WithContext(context.WithValue(req.Context(), authContextKey{}, principal{User: auth.User{ID: "usr_github_callback"}}))
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, req)
			if response.Code != tc.status || !strings.Contains(response.Body.String(), `"code":"`+tc.code+`"`) {
				t.Fatalf("callback error = %d %s", response.Code, response.Body.String())
			}
			if strings.Contains(response.Body.String(), privateDetail) || strings.Contains(logs.String(), privateDetail) {
				t.Fatal("callback response or logs disclosed provider detail")
			}
			reference := response.Header().Get("Support-Reference")
			if reference == "" || !strings.Contains(response.Body.String(), reference) {
				t.Fatal("callback response did not retain its support reference")
			}

			cancelReports()
			select {
			case <-reportsDone:
			case <-time.After(5 * time.Second):
				t.Fatal("support reporter did not drain")
			}
			store.mu.Lock()
			defer store.mu.Unlock()
			if !tc.wantCapture {
				if len(store.records) != 0 {
					t.Fatalf("expected rejection created support records: %+v", store.records)
				}
				if !strings.Contains(logs.String(), "outcome=rejected") {
					t.Fatalf("expected OAuth rejection lacked a local warning: %s", logs.String())
				}
				return
			}
			if len(store.records) != 1 {
				t.Fatalf("support records = %d; want one callback failure", len(store.records))
			}
			record := store.records[0]
			if record.Reference != reference || record.Cause != tc.cause || record.HTTPStatus != http.StatusServiceUnavailable {
				t.Fatalf("callback cause/correlation was not preserved: %+v", record)
			}
			if strings.Contains(strings.Join(record.ErrorChain, " "), privateDetail) {
				t.Fatal("support record disclosed provider detail")
			}
		})
	}
}

func TestGitHubNativeStatusReportsStoreOutage(t *testing.T) {
	store, err := db.Open(config.Database{Driver: "postgres", DSN: "postgres://paperboat:paperboat@127.0.0.1:1/paperboat_test?sslmode=disable&connect_timeout=1"})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	reportsStore := &supportTestStore{}
	reports, err := supportreport.New(reportsStore, supportreport.Config{})
	if err != nil {
		t.Fatal(err)
	}
	runCtx, cancelReports := context.WithCancel(context.Background())
	reportsDone := make(chan struct{})
	go func() { _ = reports.Run(runCtx); close(reportsDone) }()
	defer func() {
		cancelReports()
		select {
		case <-reportsDone:
		case <-time.After(5 * time.Second):
			t.Error("support reporter did not drain")
		}
	}()

	var logs strings.Builder
	handler := supportMiddleware(reports, slog.New(slog.NewTextHandler(&logs, nil)), githubNativeStatus(pbgithub.NewService(store, nil, nil, config.Default()), false))
	request := httptest.NewRequest(http.MethodGet, "/v1/github/native-links/handoff_test", nil)
	request.SetPathValue("link_id", "handoff_test")
	request = request.WithContext(context.WithValue(request.Context(), authContextKey{}, principal{User: auth.User{ID: "usr_native_status"}, Client: &auth.ClientPrincipal{SessionID: "cli_native_status"}}))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), `"code":"github_connection_unavailable"`) {
		t.Fatalf("native status store failure = %d %s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), "127.0.0.1") || !strings.Contains(logs.String(), "connection_refused") {
		t.Fatalf("native status failure lost safe local diagnostics: %s", logs.String())
	}
	reference := response.Header().Get("Support-Reference")
	if reference == "" || !strings.Contains(response.Body.String(), reference) {
		t.Fatal("native status response did not retain its support reference")
	}

	cancelReports()
	select {
	case <-reportsDone:
	case <-time.After(5 * time.Second):
		t.Fatal("support reporter did not drain")
	}
	reportsStore.mu.Lock()
	defer reportsStore.mu.Unlock()
	if len(reportsStore.records) != 1 || reportsStore.records[0].Reference != reference || reportsStore.records[0].Cause != "connection_refused" {
		t.Fatalf("native status failure was not correlated and captured once: %+v", reportsStore.records)
	}
}

func TestGitHubReturnToAcceptsOnlyConsolePaths(t *testing.T) {
	for path, want := range map[string]bool{
		"/config": true, "/config?tab=sync": true, "": false, "config": false, "//evil.example.test": false,
		"/\\evil.example.test": false, "https://evil.example.test": false, "/a\r\nSet-Cookie:x": false,
		"/" + strings.Repeat("a", 300): false,
	} {
		if consolePath(path) != want {
			t.Errorf("consolePath(%q) = %v; want %v", path, !want, want)
		}
	}
}

func TestGitHubBrowserCallbackSendsTheBrowserToTheConsole(t *testing.T) {
	cfg := config.Default()
	cfg.HTTP.BrowserLoginOrigin = "https://console.example.test"
	githubService := pbgithub.NewService(nil, nil, &pbgithub.FakeClient{}, cfg)
	// No session on the API origin: the browser must still never see an API body.
	req := httptest.NewRequest(http.MethodGet, "/v1/github/oauth/callback?code=c&state=s&error=access_denied&extra=x", nil)
	rec := httptest.NewRecorder()
	githubCallbackDispatch(githubService, githubOAuthBrowserCallback(githubService)).ServeHTTP(rec, req)
	want := "https://console.example.test/github/callback?code=c&error=access_denied&state=s"
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != want {
		t.Fatalf("callback = %d %q; want %q", rec.Code, rec.Header().Get("Location"), want)
	}
}
