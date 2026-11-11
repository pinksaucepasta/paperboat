//go:build darwin || linux

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/runtime"
	"github.com/spf13/cobra"
)

func TestEnvHookLineRunsPbByAbsolutePathForEachShell(t *testing.T) {
	for _, shell := range envHookShells {
		line := envHookLine(shell, "/opt/p b/pb")
		start := fmt.Sprintf("env hook --emit %s 2>/dev/null", shell)
		if !strings.Contains(line, "'/opt/p b/pb' ") && !strings.Contains(line, "$('/opt/p b/pb' ") || !strings.Contains(line, start) ||
			!strings.HasSuffix(line, envHookMarker) || strings.Contains(line, "\n") {
			t.Fatalf("%s line = %q", shell, line)
		}
	}
}

// hostileLoadValue is what the fake pb emits through the real quoting.
const hostileLoadValue = "it's $HOME `x` \"q\" \\n\nsecond line"

// fakeLoadPB is a pb whose `env hook --emit` prints the hostile value and a
// counter that grows on every call; anything else prints "passthrough: args".
func fakeLoadPB(t *testing.T) (dir string) {
	t.Helper()
	dir = t.TempDir()
	script := "#!/bin/sh\n" +
		"if [ \"$1 $2 $3\" = 'env hook --emit' ]; then\n" +
		"  n=$(cat \"$FAKE_COUNTER\" 2>/dev/null || echo 0); n=$((n+1)); echo $n > \"$FAKE_COUNTER\"\n" +
		"  exec \"$FAKE_PRINTER\" \"$4\" \"$n\"\n" +
		"fi\necho \"passthrough: $*\"\n"
	if err := os.WriteFile(filepath.Join(dir, "pb"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestEnvLoadFunctionLoadsIntoTheCurrentShellInEveryShell(t *testing.T) {
	for _, shell := range envHookShells {
		t.Run(shell, func(t *testing.T) {
			path, err := exec.LookPath(shell)
			if err != nil {
				t.Skipf("%s not installed", shell)
			}
			dir := fakeLoadPB(t)
			printer := filepath.Join(dir, "printer")
			printerScript := "#!/bin/sh\nq=" + shellSingleQuote(hostileLoadValue) + "\n"
			if shell == "fish" {
				printerScript += "printf 'set -gx PB_LOADED %s\\n' \"$2\"\nprintf 'set -gx PB_HOSTILE %s\\n' " + shellSingleQuote(fishSingleQuote(hostileLoadValue)) + "\n"
			} else {
				printerScript += "printf 'export PB_LOADED=%s\\n' \"$2\"\nprintf 'export PB_HOSTILE=%s\\n' " + shellSingleQuote(shellSingleQuote(hostileLoadValue)) + "\n"
			}
			if err := os.WriteFile(printer, []byte(printerScript), 0o755); err != nil {
				t.Fatal(err)
			}
			echo := "printf '<%s>' \"$PB_LOADED\"; printf '[%s]' \"$PB_HOSTILE\"; "
			if shell == "fish" {
				echo = "printf '<%s>' $PB_LOADED; printf '[%s]' $PB_HOSTILE; "
			}
			script := envHookLine(shell, filepath.Join(dir, "pb")) + "\n" +
				echo + "pb env load; " + echo + "pb status --x; pb env load extra; pb env"
			command := exec.Command(path, "-c", script)
			// pb is not on PATH (as on a Mac, ~/.local/bin): the function must still reach it.
			command.Env = append(os.Environ(), "FAKE_COUNTER="+filepath.Join(dir, "count"), "FAKE_PRINTER="+printer)
			out, err := command.CombinedOutput()
			want := "<1>[" + hostileLoadValue + "]<2>[" + hostileLoadValue + "]" +
				"passthrough: status --x\npassthrough: env load extra\npassthrough: env\n"
			if err != nil || string(out) != want {
				t.Fatalf("%s: out = %q err = %v\nwant %q", shell, out, err, want)
			}
		})
	}
}

func TestEnvHookUninstallRemovesThePbFunctionInEveryShell(t *testing.T) {
	for _, shell := range envHookShells {
		t.Run(shell, func(t *testing.T) {
			path, err := exec.LookPath(shell)
			if err != nil {
				t.Skipf("%s not installed", shell)
			}
			home := t.TempDir()
			t.Setenv("HOME", home)
			dir := fakeLoadPB(t)
			rc := envHookRCFile(home, shell)
			if _, err := writeEnvHookLine(rc, envHookLine(shell, filepath.Join(dir, "pb")), shell == "bash"); err != nil {
				t.Fatal(err)
			}
			probe := "type -t pb"
			if shell == "zsh" {
				probe = "whence -w pb"
			}
			check := func() string {
				command := exec.Command(path, "-c", "source "+rc+"; "+probe)
				command.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "HOME="+home, "FAKE_COUNTER="+filepath.Join(dir, "c"), "FAKE_PRINTER=/bin/true")
				out, _ := command.CombinedOutput()
				return strings.TrimSpace(string(out))
			}
			if got := check(); !strings.HasSuffix(got, "function") {
				t.Fatalf("installed hook should define a pb function, probe = %q", got)
			}
			root := newRootCommand()
			root.SetOut(&bytes.Buffer{})
			root.SetArgs([]string{"env", "hook", "--uninstall"})
			if err := root.Execute(); err != nil {
				t.Fatal(err)
			}
			if got := check(); !strings.HasSuffix(got, "file") && !strings.HasSuffix(got, "command") {
				t.Fatalf("after --uninstall pb must be the plain command again, probe = %q", got)
			}
		})
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
		{"names setting off", `{"error":{"code":"names_listing_disabled"}}`, 403, false, "", 0},
	}
	for _, tc := range cases {
		url := shellEnvironmentTestServer(t, tc.status, tc.body, 0)
		got := requestShellEnvironment(t.Context(), url, "token", runtime.ShellEnvironmentPath, record)
		if got.quiet != tc.quiet || len(got.values) != tc.values || !strings.Contains(got.reason, tc.reason) {
			t.Fatalf("%s: got %+v", tc.name, got)
		}
		if got.disabled != (tc.name == "names setting off") {
			t.Fatalf("%s: disabled=%v", tc.name, got.disabled)
		}
	}
}

func TestEnvHookGivesUpWithinItsBudgetWhenTheRuntimeHangs(t *testing.T) {
	url := shellEnvironmentTestServer(t, 200, `{"data":{"values":["A=1"]}}`, 10*time.Second)
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	started := time.Now()
	got := requestShellEnvironment(ctx, url, "token", runtime.ShellEnvironmentPath, envHookRecord{AccountID: "acct_1", WorkspaceID: "personal"})
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
	envHookTerminal = filepath.Join(home, "tty")
	t.Cleanup(func() { envHookTerminal = "/dev/tty" })
	if err := os.WriteFile(envHookTerminal, nil, 0o600); err != nil {
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
		notice, _ := os.ReadFile(envHookTerminal)
		if len(notice) != 0 {
			t.Fatalf("%s: terminal notice %q", shell, notice)
		}
	}
}

func TestEnvHookLineStaysSilentWhenPbIsOldOrBroken(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not installed")
	}
	old := filepath.Join(t.TempDir(), "pb")
	if err := os.WriteFile(old, []byte("#!/bin/sh\necho 'pb: The command arguments are invalid.' >&2\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(bash, "-c", envHookLine("bash", old)+"\necho ok").CombinedOutput()
	if err != nil || string(out) != "ok\n" {
		t.Fatalf("old pb leaked into shell startup: %q %v", out, err)
	}
}

func TestEnvHookEmitSkipsTheRootPreRun(t *testing.T) {
	root := newRootCommand()
	hook, _, err := root.Find([]string{"env", "hook"})
	if err != nil || hook.PersistentPreRunE == nil {
		t.Fatalf("env hook pre-run missing: %v", err)
	}
	called := false
	root.PersistentPreRunE = func(*cobra.Command, []string) error { called = true; return nil }
	_ = hook.Flags().Set("emit", "bash")
	if err := hook.PersistentPreRunE(hook, nil); err != nil || called {
		t.Fatalf("emit ran the root pre-run (update check): called=%v err=%v", called, err)
	}
	_ = hook.Flags().Set("emit", "")
	if err := hook.PersistentPreRunE(hook, nil); err != nil || !called {
		t.Fatalf("install/status must keep the root pre-run: called=%v err=%v", called, err)
	}
}

func TestEnvNamesRequestReadsNamesFromTheNamesPath(t *testing.T) {
	var path string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		_, _ = w.Write([]byte(`{"data":{"names":[{"name":"STRIPE_API_KEY","scope":"personal"}]}}`))
	}))
	defer server.Close()
	got := loadNamesForTest(t, server.URL)
	if path != runtime.ShellEnvironmentNamesPath || len(got.names) != 1 || got.names[0].Name != "STRIPE_API_KEY" || len(got.values) != 0 {
		t.Fatalf("path %q got %+v", path, got)
	}
}

func loadNamesForTest(t *testing.T, url string) envHookResult {
	t.Helper()
	return requestShellEnvironment(t.Context(), url, "token", runtime.ShellEnvironmentNamesPath, envHookRecord{AccountID: "acct_1", WorkspaceID: "personal"})
}

// The Windows console decodes native output with its OEM code page: the PowerShell assignment carries no byte above 0x7f.
func TestPowerShellAssignmentIsASCIIOnly(t *testing.T) {
	line := envHookAssignment(envHookPowerShell, "PB_EMOJI", "emoji \U0001F680 \u8239 \u00fc it's\nnext")
	for i := 0; i < len(line); i++ {
		if line[i] >= 0x80 {
			t.Fatalf("byte %#x at %d in %q", line[i], i, line)
		}
	}
	for _, want := range []string{"ConvertFromUtf32(0x1F680)", "ConvertFromUtf32(0x8239)", "ConvertFromUtf32(0xFC)", "it''s", "[char]10"} {
		if !strings.Contains(line, want) {
			t.Fatalf("%q does not contain %q", line, want)
		}
	}
}
