package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/command"
	"github.com/pinksaucepasta/paperboat/internal/config"
)

const testEnrollmentLoginToken = "0123456789ABCDEFGHIJKLMNOP"

func tokenLoginFixture(t *testing.T, issuer string) (string, config.ProfileStore) {
	t.Helper()
	dir := t.TempDir()
	isolateCommandCredentialLocation(t, dir)
	cfgPath := filepath.Join(dir, "config.json")
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ServerURL = issuer
	cfg.Auth.AllowFileFallback = true
	cfg.Auth.ProfileDir = filepath.Join(dir, "profiles")
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	store, err := config.ProfileStoreFor(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return cfgPath, store
}

func tokenLoginResponse(t *testing.T, w http.ResponseWriter) {
	t.Helper()
	writeAPIData(t, w, map[string]any{"access_token": "login-access", "refresh_token": "login-refresh", "token_type": "Bearer", "expires_in": 3600, "scope": "account:read machines:connect", "cli_client_session_id": "cls_login", "account": map[string]string{"id": "usr_login", "email": "login@example.test", "display_name": "Login"}})
}

func testTokenPrompt(t *testing.T, token string) *int {
	t.Helper()
	calls := new(int)
	old := readAuthEnrollmentToken
	readAuthEnrollmentToken = func(*command.Context) ([]byte, error) { *calls++; return []byte(token), nil }
	t.Cleanup(func() { readAuthEnrollmentToken = old })
	oldBrowser := openBrowser
	openBrowser = func(string) error { t.Error("login opened browser"); return nil }
	t.Cleanup(func() { openBrowser = oldBrowser })
	return calls
}

func runTokenLogin(t *testing.T, cfgPath string, extra ...string) (int, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	args := append([]string{"--config", cfgPath, "auth", "login"}, extra...)
	code := run(context.Background(), args, &out, &errOut)
	return code, out.String() + errOut.String()
}

func TestAuthTokenLoginAuthenticatesMachineAddWithoutInstallation(t *testing.T) {
	prompts := testTokenPrompt(t, testEnrollmentLoginToken)
	requests := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.Path)
		switch r.URL.Path {
		case "/v1/auth/enrollment/token":
			var body map[string]string
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			if body["enrollment_token"] != testEnrollmentLoginToken || len(body["verifier"]) != 43 || r.Header.Get("Authorization") != "" {
				t.Error("invalid enrollment request")
			}
			tokenLoginResponse(t, w)
		case "/v1/machine-enrollments":
			if r.Header.Get("Authorization") != "Bearer login-access" {
				t.Error("machine add did not use stored login")
			}
			var body map[string]string
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			if _, ok := body["shell"]; ok {
				t.Error("machine add still sends a shell selector")
			}
			writeAPIData(t, w, map[string]any{"id": "ume_new", "bootstrap_token": "NEWENROLLMENTTOKEN", "expires_at": time.Now().Add(time.Minute)})
		default:
			t.Errorf("unexpected installation/auth request %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	cfg, store := tokenLoginFixture(t, server.URL)
	code, out := runTokenLogin(t, cfg)
	if code != 0 || !strings.Contains(out, "Signed in as login@example.test") {
		t.Fatalf("exit=%d output=%s", code, out)
	}
	if strings.Contains(out, testEnrollmentLoginToken) || strings.Contains(out, "login-access") || strings.Contains(out, "login-refresh") {
		t.Fatal("output contains credentials")
	}
	if *prompts != 1 {
		t.Fatalf("prompts=%d", *prompts)
	}
	profile, err := store.Load(server.URL)
	if err != nil || profile.CLIClientSessionID != "cls_login" {
		t.Fatalf("profile=%v err=%v", profile, err)
	}
	if err := store.WithEnrollmentLogin(server.URL, func(state *config.EnrollmentLoginState, _ func() error) error {
		if state.Token != "" {
			t.Error("completed login kept resume token")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if code := run(context.Background(), []string{"--config", cfg, "machine", "add"}, &output, &output); code != 0 {
		t.Fatalf("machine add exit=%d", code)
	}
	if !strings.Contains(output.String(), "Linux/macOS:") || !strings.Contains(output.String(), "Windows (PowerShell or Command Prompt):") {
		t.Fatal("machine add missing a platform")
	}
	if len(requests) != 2 {
		t.Fatalf("requests=%v", requests)
	}
	command, _, err := newRootCommand().Find([]string{"machine", "add"})
	if err != nil || command.Flags().Lookup("shell") != nil {
		t.Fatal("machine add still exposes --shell")
	}
}

func TestAuthTokenLoginResumesAmbiguousExchangeWithoutAnotherPrompt(t *testing.T) {
	prompts := testTokenPrompt(t, testEnrollmentLoginToken)
	var verifier string
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if calls == 1 {
			verifier = body["verifier"]
			http.Error(w, "transient", 503)
			return
		}
		if verifier == "" || body["verifier"] != verifier || body["enrollment_token"] != testEnrollmentLoginToken {
			t.Error("retry changed grant binding")
		}
		tokenLoginResponse(t, w)
	}))
	defer server.Close()
	cfg, _ := tokenLoginFixture(t, server.URL)
	if code, _ := runTokenLogin(t, cfg); code == 0 {
		t.Fatal("ambiguous request succeeded")
	}
	if code, out := runTokenLogin(t, cfg); code != 0 {
		t.Fatalf("resume exit=%d output=%s", code, out)
	}
	if *prompts != 1 || calls != 2 {
		t.Fatalf("prompts=%d calls=%d", *prompts, calls)
	}
}

func TestAuthTokenLoginResumesAfterProfileCommitWithoutReexchange(t *testing.T) {
	testTokenPrompt(t, testEnrollmentLoginToken)
	var cfg string
	var cfgData []byte
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var err error
		cfgData, err = os.ReadFile(cfg)
		if err != nil {
			t.Error(err)
		}
		if err := os.Remove(cfg); err != nil {
			t.Error(err)
		}
		if err := os.Mkdir(cfg, 0700); err != nil {
			t.Error(err)
		}
		tokenLoginResponse(t, w)
	}))
	defer server.Close()
	cfg, store := tokenLoginFixture(t, server.URL)
	if code, _ := runTokenLogin(t, cfg); code == 0 {
		t.Fatal("configuration save should fail")
	}
	if _, err := store.Load(server.URL); err != nil {
		t.Fatalf("profile not committed: %v", err)
	}
	if err := os.Remove(cfg); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg, cfgData, 0600); err != nil {
		t.Fatal(err)
	}
	if code, out := runTokenLogin(t, cfg); code != 0 {
		t.Fatalf("resume exit=%d output=%s", code, out)
	}
	if calls != 1 {
		t.Fatalf("reexchanged committed token: %d", calls)
	}
}

func TestAuthTokenLoginRejectsInvalidAndUsedTokensWithoutEcho(t *testing.T) {
	for _, scenario := range []string{"malformed", "expired_token", "invalid_grant", "access_denied"} {
		t.Run(scenario, func(t *testing.T) {
			token := testEnrollmentLoginToken
			if scenario == "malformed" {
				token = "bad-token"
			}
			testTokenPrompt(t, token)
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.WriteHeader(400)
				_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": scenario, "message": token}})
			}))
			defer server.Close()
			cfg, store := tokenLoginFixture(t, server.URL)
			code, out := runTokenLogin(t, cfg)
			if code == 0 || strings.Contains(out, token) {
				t.Fatalf("exit=%d secret-safe=%v", code, !strings.Contains(out, token))
			}
			if scenario == "malformed" && calls != 0 {
				t.Fatal("invalid token reached server")
			}
			if _, err := store.Load(server.URL); !errors.Is(err, config.ErrNoCredentials) {
				t.Fatalf("profile after rejection: %v", err)
			}
			if err := store.WithEnrollmentLogin(server.URL, func(state *config.EnrollmentLoginState, _ func() error) error {
				if state.Token != "" {
					t.Error("rejected token retained")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestAuthTokenLoginJSONUsesProtectedFileAndNoPrompt(t *testing.T) {
	prompts := testTokenPrompt(t, "must-not-be-read")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { tokenLoginResponse(t, w) }))
	defer server.Close()
	cfg, _ := tokenLoginFixture(t, server.URL)
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte(testEnrollmentLoginToken+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	code, out := runTokenLogin(t, cfg, "--token-file", tokenFile, "--json")
	if code != 0 {
		t.Fatalf("exit=%d output=%s", code, out)
	}
	var result struct {
		OK   bool `json:"ok"`
		Data struct {
			SignedIn bool `json:"signed_in"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatal(err)
	}
	if !result.OK || !result.Data.SignedIn || *prompts != 0 {
		t.Fatalf("result=%+v prompts=%d", result, *prompts)
	}
	if strings.Contains(out, testEnrollmentLoginToken) || strings.Contains(out, "login-access") || strings.Contains(out, "login-refresh") {
		t.Fatal("JSON contains credentials")
	}
}

func TestAuthTokenLoginLogoutCancelsAmbiguousAttempt(t *testing.T) {
	testTokenPrompt(t, testEnrollmentLoginToken)
	exchanges, revokes := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/auth/enrollment/token":
			exchanges++
			if exchanges == 1 {
				http.Error(w, "transient", 503)
				return
			}
			tokenLoginResponse(t, w)
		case "/v1/auth/token/revoke":
			revokes++
			if r.Header.Get("Authorization") != "Bearer login-refresh" {
				t.Error("wrong revocation credential")
			}
			writeAPIData(t, w, map[string]any{})
		default:
			t.Errorf("unexpected endpoint %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	cfg, store := tokenLoginFixture(t, server.URL)
	if code, _ := runTokenLogin(t, cfg); code == 0 {
		t.Fatal("ambiguous exchange succeeded")
	}
	var out bytes.Buffer
	if code := run(context.Background(), []string{"--config", cfg, "auth", "logout"}, &out, &out); code != 0 {
		t.Fatalf("logout exit=%d output=%s", code, &out)
	}
	if exchanges != 2 || revokes != 1 {
		t.Fatalf("exchanges=%d revokes=%d", exchanges, revokes)
	}
	if _, err := store.Load(server.URL); !errors.Is(err, config.ErrNoCredentials) {
		t.Fatalf("logout installed profile: %v", err)
	}
	if err := store.WithEnrollmentLogin(server.URL, func(state *config.EnrollmentLoginState, _ func() error) error {
		if state.Token != "" {
			t.Error("cancelled login retained")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestAuthTokenLoginKeepsCancelledAttemptFromSigningIn(t *testing.T) {
	testTokenPrompt(t, testEnrollmentLoginToken)
	available := false
	exchanges, revokes := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/auth/token/revoke" {
			revokes++
			writeAPIData(t, w, map[string]any{})
			return
		}
		exchanges++
		if !available {
			http.Error(w, "transient", 503)
			return
		}
		tokenLoginResponse(t, w)
	}))
	defer server.Close()
	cfg, store := tokenLoginFixture(t, server.URL)
	if code, _ := runTokenLogin(t, cfg); code == 0 {
		t.Fatal("unavailable login succeeded")
	}
	var out bytes.Buffer
	if code := run(context.Background(), []string{"--config", cfg, "auth", "logout"}, &out, &out); code == 0 {
		t.Fatal("pending cancellation reported complete")
	}
	if err := store.WithEnrollmentLogin(server.URL, func(state *config.EnrollmentLoginState, _ func() error) error {
		if !state.Cancelled {
			t.Error("logout did not fence pending login")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	available = true
	out.Reset()
	if code := run(context.Background(), []string{"--config", cfg, "auth", "logout"}, &out, &out); code != 0 {
		t.Fatalf("retry logout exit=%d output=%s", code, &out)
	}
	if revokes != 1 {
		t.Fatalf("revokes=%d exchanges=%d", revokes, exchanges)
	}
	if _, err := store.Load(server.URL); !errors.Is(err, config.ErrNoCredentials) {
		t.Fatalf("cancellation installed session: %v", err)
	}
}

func TestAuthTokenLoginPreservesExistingAccountStoragePolicy(t *testing.T) {
	prompts := testTokenPrompt(t, testEnrollmentLoginToken)
	old := authCredentialStoreAvailable
	authCredentialStoreAvailable = func() bool { return false }
	defer func() { authCredentialStoreAvailable = old }()
	cfgPath, store := tokenLoginFixture(t, "https://api.example.test")
	expiry := time.Now().Add(time.Hour)
	profile := config.Profile{Issuer: "https://api.example.test", CLIClientSessionID: "cls_existing", Account: config.Account{ID: "usr_existing"}, AccessExpiresAt: expiry}
	if err := store.Save(profile, config.Credential{AccessToken: "existing-access", RefreshToken: "existing-refresh", TokenType: "Bearer", ExpiresAt: expiry}); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Auth.AllowFileFallback = false
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	code, out := runTokenLogin(t, cfgPath)
	if code == 0 || !strings.Contains(out, "already has a session") || *prompts != 0 {
		t.Fatalf("exit=%d prompts=%d", code, *prompts)
	}
	after, err := os.ReadFile(cfgPath)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("existing account storage policy changed")
	}
	current, err := store.Load(profile.Issuer)
	if err != nil || current.CLIClientSessionID != profile.CLIClientSessionID {
		t.Fatal("existing account replaced")
	}
}
