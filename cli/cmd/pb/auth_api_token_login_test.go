package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/cli/internal/config"
)

// pb login --api-token (or PAPERBOAT_API_TOKEN) exchanges the token for a CLI session and activates it like a
// browser login; the token itself is never saved.
func TestAPITokenLoginStartsATokenSession(t *testing.T) {
	const secret = "pbat_" + "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/auth/api-token-sessions":
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			if r.Header.Get("Authorization") != "Bearer "+secret || body["client_label"] == "" || body["os"] == "" {
				t.Errorf("exchange request %v %v", r.Header.Get("Authorization") != "", body)
			}
			writeAPIData(t, w, map[string]any{"access_token": "tok-access", "refresh_token": "tok-refresh", "token_type": "Bearer", "expires_in": 3600, "cli_client_session_id": "cls_token"})
		case "/v1/me":
			if r.Header.Get("Authorization") != "Bearer tok-access" {
				t.Error("me without the session credential")
			}
			writeAPIData(t, w, map[string]string{"id": "usr_token", "email": "agent@example.test", "status": "active"})
		case "/v1/e2ee/bootstrap":
			acceptBrowserPeerEnrollment(t, w, r, "usr_token", "cls_token")
		case "/v1/machines":
			writeAPIData(t, w, map[string]any{"items": []any{}, "pagination": map[string]any{"limit": 200, "offset": 0, "total": 0, "next_offset": nil}})
		case "/v1/teams":
			writeAPIData(t, w, []any{})
		case "/v1/auth/token/revoke":
			w.WriteHeader(204)
		default:
			t.Errorf("unexpected endpoint %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	old := loginHTTPClient
	t.Cleanup(func() { loginHTTPClient = old })
	loginHTTPClient = srv.Client
	path, store := browserLoginFixture(t, srv.URL)
	// A headless Mac or container has no usable credential store: the session goes to owner-only files
	// (a test Mac over SSH refused the Keychain write, exit 36).
	headless, _ := config.Load(path)
	headless.Auth.AllowFileFallback = false
	if err := headless.Save(); err != nil {
		t.Fatal(err)
	}
	oldStore := authCredentialStoreAvailable
	t.Cleanup(func() { authCredentialStoreAvailable = oldStore })
	authCredentialStoreAvailable = func() bool { return false }
	out, err := runBrowserLogin(t, context.Background(), path, "login", "--api-token", secret)
	if err != nil || !strings.Contains(out, "Signed in as agent@example.test with an API token") {
		t.Fatalf("login: %v %s", err, out)
	}
	if saved, _ := config.Load(path); !saved.Auth.AllowFileFallback {
		t.Fatal("a headless API token sign-in did not switch to owner-only files")
	}
	profile, err := store.Load(srv.URL)
	if err != nil || profile.CLIClientSessionID != "cls_token" || profile.Account.ID != "usr_token" {
		t.Fatalf("profile %+v %v", profile, err)
	}
	_ = filepath.WalkDir(filepath.Dir(path), func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if b, _ := os.ReadFile(p); strings.Contains(string(b), secret) {
				t.Errorf("the API token was saved in %s", p)
			}
		}
		return nil
	})
	if _, err := runBrowserLogin(t, context.Background(), path, "login", "--api-token", "not-a-token"); err == nil {
		t.Fatal("a value without a pbat_ token was accepted")
	}
	if _, err := runBrowserLogin(t, context.Background(), path, "login", "--api-token", secret, "--fresh"); err == nil {
		t.Fatal("--api-token combined with --fresh")
	}
	t.Setenv("PAPERBOAT_API_TOKEN", secret)
	if out, err := runBrowserLogin(t, context.Background(), path, "login"); err != nil || !strings.Contains(out, "with an API token") {
		t.Fatalf("PAPERBOAT_API_TOKEN login: %v %s", err, out)
	}
}
