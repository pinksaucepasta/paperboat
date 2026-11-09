package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/environmentmanager"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/runtime"
	"github.com/spf13/cobra"
)

func namesTestCommand() (*cobra.Command, *bytes.Buffer, *bytes.Buffer) {
	var out, errOut bytes.Buffer
	command := &cobra.Command{}
	command.SetOut(&out)
	command.SetErr(&errOut)
	return command, &out, &errOut
}

var namesFixture = []runtime.EnvironmentName{{Name: "DB_URL", Scope: "team"}, {Name: "STRIPE_API_KEY", Scope: "personal"}}

func TestEnvNamesPrintsNamesOnePerLineAndNothingElse(t *testing.T) {
	command, out, errOut := namesTestCommand()
	if err := writeEnvironmentNames(command, "personal", envHookResult{names: namesFixture}); err != nil {
		t.Fatal(err)
	}
	if out.String() != "DB_URL\nSTRIPE_API_KEY\n" || errOut.Len() != 0 {
		t.Fatalf("stdout %q stderr %q", out.String(), errOut.String())
	}
}

func TestEnvNamesJSONHasNameAndScopeOnly(t *testing.T) {
	command, out, _ := namesTestCommand()
	if err := writeEnvironmentNames(command, "team_a", envHookResult{names: namesFixture}); err != nil {
		t.Fatal(err)
	}
	command.Flags().Bool("json", false, "")
	_ = command.Flags().Set("json", "true")
	out.Reset()
	if err := writeEnvironmentNames(command, "team_a", envHookResult{names: namesFixture}); err != nil {
		t.Fatal(err)
	}
	want := `{"schema_version":"1.0","ok":true,"data":{"names":[{"name":"DB_URL","scope":"team"},{"name":"STRIPE_API_KEY","scope":"personal"}],"workspace_id":"team_a"}}` + "\n"
	if out.String() != want {
		t.Fatalf("json = %s", out.String())
	}
}

func TestEnvNamesEmptyListGoesToStderrSoPipesStayEmpty(t *testing.T) {
	command, out, errOut := namesTestCommand()
	if err := writeEnvironmentNames(command, "personal", envHookResult{}); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 || !strings.Contains(errOut.String(), "pb env set NAME --value-stdin") {
		t.Fatalf("stdout %q stderr %q", out.String(), errOut.String())
	}
}

func TestEnvNamesRefusalsSayWhatToDoAndPrintNoNames(t *testing.T) {
	for name, tc := range map[string]struct {
		result envHookResult
		want   string
	}{
		"setting off":   {envHookResult{disabled: true, names: namesFixture}, "turned off for this workspace"},
		"other account": {envHookResult{quiet: true}, "belongs to another Paperboat account"},
		"pending":       {envHookResult{reason: "ENV delivery to this machine is pending"}, "is pending"},
	} {
		command, out, _ := namesTestCommand()
		err := writeEnvironmentNames(command, "personal", tc.result)
		if err == nil || !strings.Contains(err.Error(), tc.want) || out.Len() != 0 {
			t.Fatalf("%s: err=%v stdout=%q", name, err, out.String())
		}
	}
	command, _, _ := namesTestCommand()
	err := writeEnvironmentNames(command, "personal", envHookResult{disabled: true})
	if err == nil || !strings.Contains(err.Error(), "Environment page") || !strings.Contains(err.Error(), "pb env run --") {
		t.Fatalf("setting-off message must say who can turn it on and that run still works: %v", err)
	}
}

func TestEnvNamesWithoutLoginSaysToLogInAndHelpGuidesAgents(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PAPERBOAT_CONFIG", filepath.Join(home, "config.json"))
	root := newRootCommand()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"env", "names"})
	if err := root.Execute(); err == nil || !strings.Contains(err.Error(), "ENV names not listed: log in first") {
		t.Fatalf("err = %v", err)
	}
	for _, args := range [][]string{{"env", "--help"}, {"env", "names", "--help"}} {
		out.Reset()
		root = newRootCommand()
		root.SetOut(&out)
		root.SetErr(&out)
		root.SetArgs(args)
		if err := root.Execute(); err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"pb env names", "pb env run -- <command>", "pb env set NAME --value-stdin", "Never print"} {
			if !strings.Contains(out.String(), want) {
				t.Fatalf("%v help lacks %q:\n%s", args, want, out.String())
			}
		}
	}
	if _, err := os.Stat(filepath.Join(home, "config.json")); err == nil {
		t.Fatal("listing names wrote a config")
	}
}

func TestEnvListHintPointsLockedVaultAtNames(t *testing.T) {
	for _, cause := range []error{environmentmanager.ErrVaultLocked, environmentmanager.ErrVaultNotSetUp} {
		err := withNamesHint(envCommandError(cause, "ENV vault is locked"))
		if err == nil || !strings.Contains(err.Error(), "pb env names") || !errors.Is(err, cause) {
			t.Fatalf("%v: %v", cause, err)
		}
	}
	other := envCommandError(errors.New("x"), "environment variable update failed")
	if withNamesHint(other) != other {
		t.Fatal("unrelated failures must pass through unchanged")
	}
}

// --json must be a real flag of the command: the CLI refuses --json on a command that does not declare it.
func TestEnvNamesDeclaresJSONOutput(t *testing.T) {
	command, _, err := newRootCommand().Find([]string{"env", "names"})
	if err != nil || command.Flags().Lookup("json") == nil {
		t.Fatalf("pb env names must declare --json: %v", err)
	}
	if err := prepareJSONCommand(command); err != nil {
		t.Fatal(err)
	}
}
