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
)

// pb login --api-token-file exchanges the token for a CLI session and activates it like a
// browser login; the token itself is never saved.
func TestAPITokenLoginStartsATokenSession(t *testing.T) {
	const secret = "pbat_" + "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	var srv *httptest.Server
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte(secret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := runBrowserLogin(t, context.Background(), path, "login", "--api-token-file", tokenFile)
	if err != nil || !strings.Contains(out, "Signed in as agent@example.test with an API token") {
		t.Fatalf("login: %v %s", err, out)
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
	bad := filepath.Join(t.TempDir(), "bad")
	_ = os.WriteFile(bad, []byte("not-a-token"), 0o600)
	if _, err := runBrowserLogin(t, context.Background(), path, "login", "--api-token-file", bad); err == nil {
		t.Fatal("a file without a pbat_ token was accepted")
	}
	if _, err := runBrowserLogin(t, context.Background(), path, "login", "--api-token-file", tokenFile, "--fresh"); err == nil {
		t.Fatal("--api-token-file combined with --fresh")
	}
}
