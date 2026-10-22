//go:build darwin || linux

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestEnvHookLineRunsPbByAbsolutePathForEachShell(t *testing.T) {
	for shell, want := range map[string]string{
		"bash": `[ -x '/opt/p b/pb' ] && eval "$('/opt/p b/pb' env hook --emit bash)" # paperboat-env-hook`,
		"zsh":  `[ -x '/opt/p b/pb' ] && eval "$('/opt/p b/pb' env hook --emit zsh)" # paperboat-env-hook`,
		"fish": `test -x '/opt/p b/pb'; and '/opt/p b/pb' env hook --emit fish | source # paperboat-env-hook`,
	} {
		if got := envHookLine(shell, "/opt/p b/pb"); got != want {
			t.Fatalf("%s line = %q, want %q", shell, got, want)
		}
	}
}

func TestEnvHookInstallIsIdempotentAndUninstallRemovesOnlyItsLine(t *testing.T) {
	rc := filepath.Join(t.TempDir(), ".bashrc")
	original := "case $- in *i*) ;; *) return;; esac\nalias ll='ls -l'\n"
	if err := os.WriteFile(rc, []byte(original), 0o640); err != nil {
		t.Fatal(err)
	}
	line := envHookLine("bash", "/usr/bin/pb")
	if changed, err := writeEnvHookLine(rc, line, true); err != nil || !changed {
		t.Fatalf("first install changed=%v err=%v", changed, err)
	}
	if changed, err := writeEnvHookLine(rc, line, true); err != nil || changed {
		t.Fatalf("second install changed=%v err=%v", changed, err)
	}
	data, _ := os.ReadFile(rc)
	if string(data) != line+"\n"+original {
		t.Fatalf("bash hook must be the first line, before the non-interactive return:\n%s", data)
	}
	moved := envHookLine("bash", "/opt/pb")
	if changed, err := writeEnvHookLine(rc, moved, true); err != nil || !changed {
		t.Fatalf("moved pb changed=%v err=%v", changed, err)
	}
	data, _ = os.ReadFile(rc)
	if strings.Count(string(data), envHookMarker) != 1 || !strings.HasPrefix(string(data), moved+"\n") {
		t.Fatalf("hook line not replaced in place:\n%s", data)
	}
	kept, found := removeEnvHookLines(string(data))
	if found != 1 || kept != original {
		t.Fatalf("uninstall kept %q found %d", kept, found)
	}
	if info, _ := os.Stat(rc); info.Mode().Perm() != 0o640 {
		t.Fatalf("rc mode changed to %v", info.Mode().Perm())
	}
}

func TestEnvHookAppendsForZshAndFishAndCreatesMissingFile(t *testing.T) {
	rc := filepath.Join(t.TempDir(), ".config", "fish", "config.fish")
	line := envHookLine("fish", "/usr/bin/pb")
	if _, err := writeEnvHookLine(rc, line, false); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(rc)
	if string(data) != line+"\n" {
		t.Fatalf("fish rc = %q", data)
	}
	zsh := filepath.Join(t.TempDir(), ".zshenv")
	_ = os.WriteFile(zsh, []byte("export A=1"), 0o600)
	if _, err := writeEnvHookLine(zsh, envHookLine("zsh", "/usr/bin/pb"), false); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(zsh)
	if !strings.HasPrefix(string(data), "export A=1\n[ -x") {
		t.Fatalf("zsh rc = %q", data)
	}
}

func TestEnvHookQuotingSurvivesTheRealShell(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not installed")
	}
	value := "it's $HOME `x` \"q\" \\n\nsecond line"
	script := "export PB_HOOK_TEST=" + shellSingleQuote(value) + "\nprintf %s \"$PB_HOOK_TEST\""
	out, err := exec.Command(bash, "-c", script).Output()
	if err != nil || string(out) != value {
		t.Fatalf("bash round trip = %q, %v", out, err)
	}
	if fish, err := exec.LookPath("fish"); err == nil {
		out, err := exec.Command(fish, "-c", "set -gx PB_HOOK_TEST "+fishSingleQuote(value)+"; printf %s $PB_HOOK_TEST").Output()
		if err != nil || string(out) != value {
			t.Fatalf("fish round trip = %q, %v", out, err)
		}
	}
}

func shellEnvironmentTestServer(t *testing.T, status int, body string, delay time.Duration) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var in map[string]string
		if json.NewDecoder(r.Body).Decode(&in) != nil || in["workspace_id"] != "personal" || in["account_id"] != "acct_1" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	return server.URL
}

func TestEnvHookRequestOutcomes(t *testing.T) {
	record := envHookRecord{Schema: envHookSchema, AccountID: "acct_1", WorkspaceID: "personal"}
	cases := []struct {
		name, body string
		status     int
		quiet      bool
		reason     string
		values     int
	}{
		{"values", `{"data":{"values":["A=1","B=2"]}}`, 200, false, "", 2},
		{"locked or pending", `{"error":{"code":"environment_pending"}}`, 503, false, "pending", 0},
		{"undeliverable", `{"error":{"code":"environment_undeliverable"}}`, 503, false, "failed", 0},
		{"other account", `{"error":{"code":"account_mismatch"}}`, 403, true, "", 0},
	}
	for _, tc := range cases {
		url := shellEnvironmentTestServer(t, tc.status, tc.body, 0)
		got := requestShellEnvironment(t.Context(), url, "token", record)
		if got.quiet != tc.quiet || len(got.values) != tc.values || !strings.Contains(got.reason, tc.reason) {
			t.Fatalf("%s: got %+v", tc.name, got)
		}
	}
}

func TestEnvHookGivesUpWithinItsBudgetWhenTheRuntimeHangs(t *testing.T) {
	url := shellEnvironmentTestServer(t, 200, `{"data":{"values":["A=1"]}}`, 10*time.Second)
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	started := time.Now()
	got := requestShellEnvironment(ctx, url, "token", envHookRecord{AccountID: "acct_1", WorkspaceID: "personal"})
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("hook waited %v", elapsed)
	}
	if len(got.values) != 0 || !strings.Contains(got.reason, "in time") {
		t.Fatalf("got %+v", got)
	}
}

func TestEnvHookEmitPrintsNothingWithoutLoginOrRuntime(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("PAPERBOAT_RUNTIME_STATE_ROOT", filepath.Join(home, "missing-runtime"))
	configPath := filepath.Join(home, ".config", "paperboat", "config.json")
	t.Setenv("PAPERBOAT_CONFIG", configPath)
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		t.Fatal(err)
	}
	record, _ := json.Marshal(envHookRecord{Schema: envHookSchema, ServerURL: "https://api.example.test", AccountID: "acct_1", WorkspaceID: "personal"})
	if err := os.WriteFile(filepath.Join(filepath.Dir(configPath), "env-hook.json"), record, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, shell := range []string{"bash", "fish", "sh"} {
		root := newRootCommand()
		var stdout, stderr bytes.Buffer
		root.SetOut(&stdout)
		root.SetErr(&stderr)
		root.SetArgs([]string{"env", "hook", "--emit", shell})
		started := time.Now()
		if err := root.Execute(); err != nil {
			t.Fatalf("%s: emit must not fail the shell: %v", shell, err)
		}
		if stdout.Len() != 0 || stderr.Len() != 0 || time.Since(started) > envHookBudget {
			t.Fatalf("%s: stdout=%q stderr=%q", shell, stdout.String(), stderr.String())
		}
	}
}
