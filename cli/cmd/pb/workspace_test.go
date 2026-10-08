package main

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/command"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/selector"
)

func TestSwitchWorkspaceValidatesAndPersistsServerBoundDefault(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/workspaces" || r.URL.RawQuery != "" {
			t.Fatalf("workspace request = %s %s, want unscoped GET", r.Method, r.URL.RequestURI())
		}
		writeAPIData(t, w, map[string]any{"items": []map[string]string{
			{"id": "personal", "name": "Personal", "kind": "personal", "role": "owner"},
			{"id": "team-a", "name": "Team A", "kind": "team", "role": "member"},
		}})
	}))
	defer server.Close()
	writeWorkspaceAuthProfile(t, dir, configPath, server.URL, "account-1")
	t.Setenv("PAPERBOAT_WORKSPACE", "")

	var output bytes.Buffer
	if code := run(context.Background(), []string{"--config", configPath, "switch", "team-a"}, &output, &output); code != 0 {
		t.Fatalf("exit=%d output=%q", code, output.String())
	}
	if got := output.String(); got == "" || !bytes.Contains(output.Bytes(), []byte("Team A")) {
		t.Fatalf("switch output=%q, want selected team", got)
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	selected, err := loadWorkspaceDefault(cfg, server.URL, "account-1")
	if err != nil || selected != "team-a" {
		t.Fatalf("saved workspace=%q err=%v", selected, err)
	}
	if selected, err := loadWorkspaceDefault(cfg, server.URL, "account-2"); err != nil || selected != "" {
		t.Fatalf("other account workspace=%q err=%v, want no saved default", selected, err)
	}
	if selected, err := loadWorkspaceDefault(cfg, "https://other.example.test", "account-1"); err != nil || selected != "" {
		t.Fatalf("other server workspace=%q err=%v, want no saved default", selected, err)
	}
}

func TestSwitchWorkspaceRejectsUnlistedSelectorWithoutChangingDefault(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeAPIData(t, w, map[string]any{"items": []map[string]string{{"id": "personal", "name": "Personal", "kind": "personal", "role": "owner"}}})
	}))
	defer server.Close()
	writeWorkspaceAuthProfile(t, dir, configPath, server.URL, "account-1")
	t.Setenv("PAPERBOAT_WORKSPACE", "")

	var output bytes.Buffer
	if code := run(context.Background(), []string{"--config", configPath, "switch", "team-unknown"}, &output, &output); code == 0 {
		t.Fatalf("switch unexpectedly succeeded: %q", output.String())
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if selected, err := loadWorkspaceDefault(cfg, server.URL, "account-1"); err != nil || selected != "" {
		t.Fatalf("failed switch changed saved workspace to %q (err=%v)", selected, err)
	}
}

func TestSwitchWorkspaceCancellationDoesNotPersist(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/workspaces" || r.URL.RawQuery != "" {
			t.Fatalf("workspace request = %s %s, want unscoped GET", r.Method, r.URL.RequestURI())
		}
		writeAPIData(t, w, map[string]any{"items": []map[string]string{
			{"id": "personal", "name": "Personal", "kind": "personal", "role": "owner"},
			{"id": "team-a", "name": "Team A", "kind": "team", "role": "member"},
		}})
	}))
	defer server.Close()
	writeWorkspaceAuthProfile(t, dir, configPath, server.URL, "account-1")
	t.Setenv("PAPERBOAT_WORKSPACE", "")

	oldTerminal, oldChoose := workspaceTerminal, chooseWorkspace
	workspaceTerminal = func() bool { return true }
	chooseWorkspace = func(options selector.Options) (selector.Item, error) {
		if len(options.Items) != 2 {
			t.Fatalf("workspace picker items=%d, want Personal and Team A", len(options.Items))
		}
		return selector.Item{}, selector.ErrCanceled
	}
	t.Cleanup(func() {
		workspaceTerminal, chooseWorkspace = oldTerminal, oldChoose
	})

	var output bytes.Buffer
	if code := run(context.Background(), []string{"--config", configPath, "switch"}, &output, &output); code != 0 {
		t.Fatalf("canceled switch exit=%d output=%q", code, output.String())
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if selected, err := loadWorkspaceDefault(cfg, server.URL, "account-1"); err != nil || selected != "" {
		t.Fatalf("canceled switch persisted workspace %q (err=%v)", selected, err)
	}
	parent := newRootCommand()
	parent.SetContext(context.Background())
	if err := parent.PersistentFlags().Set("config", configPath); err != nil {
		t.Fatal(err)
	}
	parent.SetOut(&output)
	parent.SetErr(&output)
	if err := runHomeAction(parent, "switch-workspace"); !errors.Is(err, selector.ErrCanceled) || errors.Is(err, selector.ErrInterrupted) {
		t.Fatalf("home switch cancel must navigate back, got %v", err)
	}
	if selected, err := loadWorkspaceDefault(cfg, server.URL, "account-1"); err != nil || selected != "" {
		t.Fatal("home cancel changed active workspace", err)
	}
}

func TestWorkspaceOverridePrecedence(t *testing.T) {
	t.Setenv("PAPERBOAT_WORKSPACE", "team-env")
	root := newRootCommand()
	if err := root.PersistentFlags().Set("workspace", "team-flag"); err != nil {
		t.Fatal(err)
	}
	selected, err := resolveWorkspaceInvocation(root)
	if err != nil || selected != "team-flag" {
		t.Fatalf("flag selection=%q err=%v, want team-flag", selected, err)
	}

	root = newRootCommand()
	selected, err = resolveWorkspaceInvocation(root)
	if err != nil || selected != "team-env" {
		t.Fatalf("environment selection=%q err=%v, want team-env", selected, err)
	}
}

func TestPersistedWorkspaceDefaultIsFallbackForItsAccountAndServer(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	serverURL := "https://api.example.test"
	writeWorkspaceAuthProfile(t, dir, configPath, serverURL, "account-1")
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := saveWorkspaceDefault(cfg, serverURL, "account-1", "team-saved"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PAPERBOAT_WORKSPACE", "")
	root := newRootCommand()
	if err := root.PersistentFlags().Set("config", configPath); err != nil {
		t.Fatal(err)
	}
	selected, err := resolveWorkspaceInvocation(root)
	if err != nil || selected != "team-saved" {
		t.Fatalf("saved workspace=%q err=%v, want team-saved", selected, err)
	}

	root = newRootCommand()
	if err := root.PersistentFlags().Set("config", configPath); err != nil {
		t.Fatal(err)
	}
	if err := root.PersistentFlags().Set("server", "https://other.example.test"); err != nil {
		t.Fatal(err)
	}
	selected, err = resolveWorkspaceInvocation(root)
	if err != nil || selected != "personal" {
		t.Fatalf("other server selection=%q err=%v, want personal", selected, err)
	}
}

func TestWorkspaceScopeIsCapturedOnceForResourceRequests(t *testing.T) {
	t.Setenv("PAPERBOAT_WORKSPACE", "team-a")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/machines" || r.URL.Query().Get("workspace") != "team-a" {
			t.Fatalf("machine request = %s %s, want captured team-a scope", r.Method, r.URL.RequestURI())
		}
		writeAPIData(t, w, map[string]any{"items": []any{}, "pagination": map[string]any{"limit": 200, "offset": 0, "total": 0}})
	}))
	defer server.Close()

	root := newRootCommand()
	root.SetContext(context.Background())
	if err := captureWorkspaceInvocation(root, nil); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PAPERBOAT_WORKSPACE", "team-b")
	client, err := newWorkspaceAPIClient(actionContext(root, nil), server.URL, config.Credential{AccessToken: "workspace-test-token"})
	if err != nil {
		t.Fatal(err)
	}
	if client.Workspace() != "team-a" {
		t.Fatalf("client workspace=%q, want invocation's captured team-a", client.Workspace())
	}
	if _, err := client.ListUserMachines(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestDirectSwitchAndAuthDoNotInheritWorkspaceOverrideFailures(t *testing.T) {
	t.Setenv("PAPERBOAT_WORKSPACE", "not a valid selector")
	root := newRootCommand()
	switchCommand, _, err := root.Find([]string{"switch", "personal"})
	if err != nil {
		t.Fatal(err)
	}
	switchCommand.SetContext(context.Background())
	if err := captureWorkspaceInvocation(switchCommand, []string{"personal"}); err != nil {
		t.Fatalf("direct Personal recovery switch rejected override: %v", err)
	}
	if selected, ok := workspaceFromContext(switchCommand.Context()); !ok || selected != "personal" {
		t.Fatalf("switch captured workspace=%q, present=%t", selected, ok)
	}

	root = newRootCommand()
	authCommand, _, err := root.Find([]string{"auth", "status"})
	if err != nil {
		t.Fatal(err)
	}
	if err := captureWorkspaceInvocation(authCommand, nil); err != nil {
		t.Fatalf("auth command inherited workspace override failure: %v", err)
	}
	if _, ok := workspaceFromContext(authCommand.Context()); ok {
		t.Fatal("auth command inherited a resource workspace")
	}
}

func TestWorkspaceLastEnvironmentIsIsolatedByWorkspace(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(configPath, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	teamAPath, err := workspacePreferencePath(loaded, "https://api.example.test", "account-1", "machine:team-a")
	if err != nil {
		t.Fatal(err)
	}
	teamBPath, err := workspacePreferencePath(loaded, "https://api.example.test", "account-1", "machine:team-b")
	if err != nil {
		t.Fatal(err)
	}
	if teamAPath == teamBPath {
		t.Fatal("different workspaces share a last-environment preference")
	}
	if err := writeWorkspaceRecord(teamAPath, workspaceEnvironmentFile{Version: workspacePreferenceVersion, MachineID: "machine-a"}); err != nil {
		t.Fatal(err)
	}
	if got, err := workspaceLastEnvironment(loaded, "https://api.example.test", "account-1", "team-a"); err != nil || got != "machine-a" {
		t.Fatalf("team-a last environment=%q err=%v", got, err)
	}
	if got, err := workspaceLastEnvironment(loaded, "https://api.example.test", "account-1", "team-b"); err != nil || got != "" {
		t.Fatalf("team-b last environment=%q err=%v, want empty", got, err)
	}
}

func TestWorkspaceLastEnvironmentIgnoresUnscopedLegacyConfig(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(configPath, []byte(`{"last_environment_id":"legacy-machine"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := workspaceLastEnvironment(loaded, "https://api.example.test", "account-1", "personal"); err != nil || got != "" {
		t.Fatalf("Personal last environment=%q err=%v, want no unscoped legacy value", got, err)
	}
}

func TestRememberWorkspaceEnvironmentPersistsOnlyInSelectedWorkspace(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	serverURL := "https://api.example.test"
	writeWorkspaceAuthProfile(t, dir, configPath, serverURL, "account-1")
	loaded, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	commandContext := &command.Context{Context: context.WithValue(context.Background(), workspaceInvocationKey{}, "team-a")}
	if err := rememberWorkspaceEnvironment(commandContext, loaded, "machine-team-a"); err != nil {
		t.Fatal(err)
	}
	if got, err := workspaceLastEnvironment(loaded, serverURL, "account-1", "team-a"); err != nil || got != "machine-team-a" {
		t.Fatalf("team-a remembered environment=%q err=%v", got, err)
	}
	if got, err := workspaceLastEnvironment(loaded, serverURL, "account-1", "personal"); err != nil || got != "" {
		t.Fatalf("Personal remembered environment=%q err=%v, want isolated scope", got, err)
	}
}

func writeWorkspaceAuthProfile(t *testing.T, dir, configPath, serverURL, accountID string) {
	t.Helper()
	isolateCommandCredentialLocation(t, dir)
	profileDir := filepath.Join(dir, "credentials")
	configJSON := `{"server_url":` + quote(serverURL) + `,"auth":{"allow_file_fallback":true,"profile_dir":` + quote(profileDir) + `}}`
	if err := os.WriteFile(configPath, []byte(configJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	store := config.ProfileStore{Path: profileDir, Secrets: config.FileSecretStore{Dir: filepath.Join(profileDir, "secrets")}}
	expires := time.Now().Add(time.Hour)
	err := store.Save(config.Profile{
		Issuer: serverURL, Account: config.Account{ID: accountID}, CLIClientSessionID: "cls_workspace_test", AccessExpiresAt: expires,
	}, config.Credential{AccessToken: "workspace-test-token", RefreshToken: "workspace-test-refresh", ExpiresAt: expires})
	if err != nil {
		t.Fatal(err)
	}
}
