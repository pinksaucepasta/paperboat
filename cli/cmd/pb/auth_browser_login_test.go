package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/config"
)

func browserLoginFixture(t *testing.T, issuer string) (string, config.ProfileStore) {
	t.Helper()
	dir := t.TempDir()
	isolateCommandCredentialLocation(t, dir)
	path := filepath.Join(dir, "config.json")
	c, e := config.Load(path)
	if e != nil {
		t.Fatal(e)
	}
	c.ServerURL = issuer
	c.Auth.AllowFileFallback = true
	c.Auth.ProfileDir = filepath.Join(dir, "profiles")
	if e = c.Save(); e != nil {
		t.Fatal(e)
	}
	s, e := config.ProfileStoreFor(c)
	if e != nil {
		t.Fatal(e)
	}
	return path, s
}
func runBrowserLogin(t *testing.T, ctx context.Context, path string, args ...string) (string, error) {
	t.Helper()
	r := newRootCommand()
	var b bytes.Buffer
	r.SetOut(&b)
	r.SetErr(&b)
	r.SetArgs(append([]string{"--config", path}, args...))
	e := r.ExecuteContext(ctx)
	return b.String(), e
}
func TestBrowserLoginAliasesApprovalAndRecovery(t *testing.T) {
	code := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))
	polls := 0
	authorizations := 0
	cancelled := false
	failMe := true
	var srv *httptest.Server
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/auth/device/authorize":
			authorizations++
			writeAPIData(t, w, map[string]any{"device_code": code, "user_code": "ABCD-EFGH", "verification_uri": srv.URL + "/cli/authorize", "verification_uri_complete": srv.URL + "/cli/authorize?code=ABCD-EFGH", "expires_in": 600, "interval": 1})
		case "/v1/auth/device/token":
			polls++
			if polls == 1 {
				w.WriteHeader(400)
				json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": "authorization_pending"}})
				return
			}
			writeAPIData(t, w, map[string]any{"access_token": "test-access", "refresh_token": "test-refresh", "token_type": "Bearer", "expires_in": 3600, "cli_client_session_id": "cls_browser"})
		case "/v1/me":
			if failMe {
				failMe = false
				w.WriteHeader(503)
				return
			}
			if r.Header.Get("Authorization") != "Bearer test-access" {
				t.Error("missing credential")
			}
			writeAPIData(t, w, map[string]string{"id": "usr_browser", "email": "browser@example.test", "status": "active"})
		case "/v1/auth/device/cancel":
			cancelled = true
			w.WriteHeader(204)
		case "/v1/auth/token/revoke":
			w.WriteHeader(204)
		default:
			t.Errorf("unexpected endpoint %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	oldHTTP, oldWait, oldBrowser := loginHTTPClient, loginWait, openBrowser
	t.Cleanup(func() { loginHTTPClient, loginWait, openBrowser = oldHTTP, oldWait, oldBrowser })
	loginHTTPClient = srv.Client
	loginWait = func(context.Context, time.Duration) error { return nil }
	openBrowser = func(string) error { t.Fatal("headless login opened browser"); return nil }
	path, store := browserLoginFixture(t, srv.URL)
	out, e := runBrowserLogin(t, context.Background(), path, "login", "--no-browser")
	if e == nil || !strings.Contains(out, "/cli/authorize?code=ABCD-EFGH") || strings.Contains(out, code) {
		t.Fatalf("first login error=%v output=%s", e, out)
	}
	if _, e = store.Load(srv.URL); !errors.Is(e, config.ErrNoCredentials) {
		t.Fatalf("unvalidated session saved: %v", e)
	}
	out, e = runBrowserLogin(t, context.Background(), path, "auth", "login", "--no-browser")
	if e != nil {
		t.Fatal(e)
	}
	p, e := store.Load(srv.URL)
	if e != nil || p.Account.ID != "usr_browser" || p.CLIClientSessionID != "cls_browser" || authorizations != 1 || cancelled {
		t.Fatalf("recovery failed profile=%+v err=%v grants=%d canceled=%v", p, e, authorizations, cancelled)
	}
	if !strings.Contains(out, "Signed in as browser@example.test") {
		t.Fatal(out)
	}
	if _, e = runBrowserLogin(t, context.Background(), path, "login", "--json"); e == nil {
		t.Fatal("existing account unexpectedly replaced")
	}
}
func TestBrowserLoginCancelPreservesExistingAccount(t *testing.T) {
	code := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{8}, 32))
	cancelled := false
	var srv *httptest.Server
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/auth/device/authorize":
			writeAPIData(t, w, map[string]any{"device_code": code, "user_code": "ABCD-EFGH", "verification_uri": srv.URL + "/cli/authorize", "verification_uri_complete": srv.URL + "/cli/authorize?code=ABCD-EFGH", "expires_in": 600, "interval": 1})
		case "/v1/auth/device/token":
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": "authorization_pending"}})
		case "/v1/auth/device/cancel":
			cancelled = true
			w.WriteHeader(204)
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	oldHTTP, oldWait := loginHTTPClient, loginWait
	t.Cleanup(func() { loginHTTPClient, loginWait = oldHTTP, oldWait })
	loginHTTPClient = srv.Client
	ctx, cancel := context.WithCancel(context.Background())
	loginWait = func(context.Context, time.Duration) error { cancel(); return context.Canceled }
	path, store := browserLoginFixture(t, srv.URL)
	if e := store.Save(config.Profile{Issuer: srv.URL, Account: config.Account{ID: "usr_old"}, CLIClientSessionID: "cls_old"}, config.Credential{AccessToken: "old-access", RefreshToken: "old-refresh"}); e != nil {
		t.Fatal(e)
	}
	_, e := runBrowserLogin(t, ctx, path, "auth", "login", "--change-account", "--no-browser")
	if !errors.Is(e, context.Canceled) || !cancelled {
		t.Fatalf("cancel err=%v called=%v", e, cancelled)
	}
	p, e := store.Load(srv.URL)
	if e != nil || p.Account.ID != "usr_old" {
		t.Fatal("old login lost")
	}
	if e = store.WithBrowserLogin(srv.URL, func(s *config.BrowserLoginState, _ func() error) error {
		if s.DeviceCode != "" {
			t.Error("canceled recovery retained")
		}
		return nil
	}); e != nil {
		t.Fatal(e)
	}
}
func TestBrowserLoginCommandContracts(t *testing.T) {
	r := newRootCommand()
	for _, path := range [][]string{{"login"}, {"auth", "login"}, {"switch"}, {"auth", "switch"}} {
		c, _, e := r.Find(path)
		if e != nil || c.Name() != path[len(path)-1] {
			t.Fatalf("missing %v", path)
		}
	}
	c, _, _ := r.Find([]string{"auth", "login"})
	if c.Flags().Lookup("token-file") != nil {
		t.Fatal("retired token login exposed")
	}
}
