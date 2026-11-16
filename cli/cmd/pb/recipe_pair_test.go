package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/command"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/recipekey"
	"github.com/pinksaucepasta/paperboat/shared/recipe"
)

// pb pair pbr_… starts a recipe session (only the token id and auth leave the computer), saves it as
// this computer's sign-in, then runs the normal setup; the token itself is never saved (F26-02).
func TestRecipePairStartsARecipeSessionThenSetup(t *testing.T) {
	token, err := recipe.NewToken("")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/auth/recipe-sessions":
			var body struct {
				TokenID string `json:"token_id"`
				Auth    []byte `json:"auth"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if r.Header.Get("Authorization") != "" || body.TokenID != token.ID || !bytes.Equal(body.Auth, token.Auth()) {
				t.Errorf("session request %q %+v", r.Header.Get("Authorization"), body)
			}
			writeAPIData(t, w, map[string]any{"access_token": "rcp-access", "refresh_token": "rcp-refresh", "token_type": "Bearer", "expires_in": 3600, "cli_client_session_id": "cls_recipe",
				"recipe": map[string]any{"id": "rcp_1", "name": "ci", "revision": 1, "workspace": "personal", "recipe": map[string]any{}}})
		case "/v1/me":
			writeAPIData(t, w, map[string]string{"id": "usr_recipe", "email": "ana@example.test", "status": "active"})
		case "/v1/e2ee/bootstrap":
			acceptBrowserPeerEnrollment(t, w, r, "usr_recipe", "cls_recipe")
		case "/v1/machines":
			writeAPIData(t, w, map[string]any{"items": []any{}, "pagination": map[string]any{"limit": 200, "offset": 0, "total": 0, "next_offset": nil}})
		case "/v1/teams":
			writeAPIData(t, w, []any{})
		default:
			t.Errorf("unexpected endpoint %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	oldClient, oldMachine, oldSetup := loginHTTPClient, loginLocalMachineID, runRecipeSetup
	t.Cleanup(func() { loginHTTPClient, loginLocalMachineID, runRecipeSetup = oldClient, oldMachine, oldSetup })
	loginHTTPClient = srv.Client
	machineID := ""
	loginLocalMachineID = func(*config.Config) (string, error) { return machineID, nil }
	var setupArgs []string
	runRecipeSetup = func(_ *command.Context, args []string) int { setupArgs = args; return 0 }
	path, _ := browserLoginFixture(t, srv.URL)
	// A headless machine or container has no OS credential store: the recipe session goes to owner-only
	// files, as a browser sign-in does (F26-02 on rcp-l, 2026-11-16: "OS credential store unavailable").
	headless, _ := config.Load(path)
	headless.Auth.AllowFileFallback = false
	if err := headless.Save(); err != nil {
		t.Fatal(err)
	}
	oldStore := authCredentialStoreAvailable
	t.Cleanup(func() { authCredentialStoreAvailable = oldStore })
	authCredentialStoreAvailable = func() bool { return false }

	t.Setenv("PB_RECIPE", "")
	out, err := runBrowserLogin(t, context.Background(), path, "pair", token.String(), "--name", "runner-1")
	if err != nil || !strings.Contains(out, "Pairing this machine from recipe ci") {
		t.Fatalf("pair: %v %s", err, out)
	}
	if strings.Join(setupArgs, " ") != "--config "+path+" setup --name runner-1" {
		t.Fatalf("setup args = %v", setupArgs)
	}
	saved, _ := config.Load(path)
	store, _ := config.ProfileStoreFor(saved)
	if !saved.Auth.AllowFileFallback {
		t.Fatal("a headless pairing did not switch to owner-only files")
	}
	profile, err := store.Load(srv.URL)
	if err != nil || profile.CLIClientSessionID != "cls_recipe" || profile.Account.ID != "usr_recipe" {
		t.Fatalf("profile %+v %v", profile, err)
	}
	cfg, _ := config.Load(path)
	if saved, ok, err := recipekey.Load(cfg.Path()); err != nil || !ok || saved.TokenID != token.ID || !bytes.Equal(saved.PrivateKey, token.PrivateKey()) {
		t.Fatalf("recipe key %v %v", ok, err)
	}
	secret := token.String()[len(recipe.TokenPrefix)+11:]
	_ = filepath.WalkDir(filepath.Dir(path), func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if b, _ := os.ReadFile(p); strings.Contains(string(b), secret) {
				t.Errorf("the recipe token was saved in %s", p)
			}
		}
		return nil
	})

	// Run again on the paired machine: nothing to do, no session, no setup.
	machineID, setupArgs = "machine_1", nil
	t.Setenv("PB_RECIPE", token.String())
	out, err = runBrowserLogin(t, context.Background(), path, "pair")
	if err != nil || !strings.Contains(out, "already paired") || setupArgs != nil {
		t.Fatalf("rerun: %v %s %v", err, out, setupArgs)
	}
}

// A malformed token is refused before any request; a recipe token and an enrollment token together are refused.
func TestRecipePairRefusesBadInputAtOnce(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request %s", r.URL.Path)
	}))
	defer srv.Close()
	path, _ := browserLoginFixture(t, srv.URL)
	t.Setenv("PB_RECIPE", "")
	out, err := runBrowserLogin(t, context.Background(), path, "pair", "pbr_short")
	if err == nil || !strings.Contains(err.Error()+out, "not a recipe token") {
		t.Fatalf("malformed: %v %s", err, out)
	}
	token, _ := recipe.NewToken("")
	if _, err := runBrowserLogin(t, context.Background(), path, "pair", token.String(), "--enrollment-token", "x"); err == nil || !strings.Contains(err.Error(), "not both") {
		t.Fatalf("both tokens: %v", err)
	}
	t.Setenv("PB_RECIPE", "garbage")
	if _, err := runBrowserLogin(t, context.Background(), path, "pair"); err == nil || !strings.Contains(err.Error(), "not a recipe token") {
		t.Fatalf("PB_RECIPE malformed: %v", err)
	}
}

func TestHostMachineNameKeepsTheFirstLabel(t *testing.T) {
	for host, want := range map[string]string{"Manageds-Virtual-Machine.local": "Manageds-Virtual-Machine", "rcp-l": "rcp-l", " apple.lan ": "apple"} {
		if got := hostMachineName(host); got != want {
			t.Fatalf("%q -> %q, want %q", host, got, want)
		}
	}
}

// A rotated token is refused with 401 recipe_replaced: pb pair says the token was replaced and which
// recipe to rotate, not that a session must log in again (F26-07 device check, victus).
func TestRecipePairNamesAReplacedToken(t *testing.T) {
	token, _ := recipe.NewToken("")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"ok":false,"error":{"code":"recipe_replaced","message":"x","details":{"recipe":"ci","at":"2026-11-16T10:00:00Z"}}}`))
	}))
	defer srv.Close()
	_, err := api.StartRecipeSession(context.Background(), srv.URL, token, "l", "cli", "linux", srv.Client())
	got := commandFailureMessage(err, classifyCommandFailure(err))
	if !strings.Contains(got, "token was replaced") || !strings.Contains(got, "pb recipe rotate ci") {
		t.Fatalf("message = %q", got)
	}
}
