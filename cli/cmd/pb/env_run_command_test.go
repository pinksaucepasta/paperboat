package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func runTestCommand() (*cobra.Command, *bytes.Buffer) {
	var out bytes.Buffer
	command := &cobra.Command{}
	command.SetOut(&out)
	command.SetErr(&out)
	command.SetIn(strings.NewReader("from stdin"))
	return command, &out
}

func TestEnvRunInjectsValuesAndPassesStdioThrough(t *testing.T) {
	command, out := runTestCommand()
	t.Setenv("PB_RUN_KEEP", "kept")
	err := runWithEnvironment(command, []string{"sh", "-c", `printf '%s|%s|%s|' "$PB_RUN_KEEP" "$PB_RUN_VALUE" "$(cat)"`}, []string{"PB_RUN_VALUE=it's $x\nline"})
	if err != nil || out.String() != "kept|it's $x\nline|from stdin|" {
		t.Fatalf("out = %q, err = %v", out.String(), err)
	}
}

func TestEnvRunPassesExitCodesAndSignalsThrough(t *testing.T) {
	for script, want := range map[string]int{"exit 7": 7, "kill -TERM $$": 143} {
		command, _ := runTestCommand()
		err := runWithEnvironment(command, []string{"sh", "-c", script}, nil)
		var exit exitCodeError
		if !errors.As(err, &exit) || exit.code != want {
			t.Fatalf("%q: err = %v, want exit %d", script, err, want)
		}
	}
	command, _ := runTestCommand()
	if err := runWithEnvironment(command, []string{"true"}, nil); err != nil {
		t.Fatalf("exit 0 must be nil: %v", err)
	}
}

func TestEnvRunDoesNotRunTheCommandWhenEnvCannotLoad(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PAPERBOAT_CONFIG", filepath.Join(home, "config.json"))
	marker := filepath.Join(home, "ran")
	root := newRootCommand()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"env", "run", "--", "touch", marker})
	err := root.Execute()
	if err == nil || !strings.Contains(err.Error(), "ENV not loaded: log in first") || !strings.Contains(err.Error(), "did not run") {
		t.Fatalf("err = %v", err)
	}
	if _, statErr := os.Stat(marker); statErr == nil {
		t.Fatal("the command ran although ENV could not load")
	}
	root = newRootCommand()
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"env", "run", "touch", marker})
	if err := root.Execute(); err == nil || !strings.Contains(err.Error(), "put -- before the command") {
		t.Fatalf("missing -- err = %v", err)
	}
}

func TestEnvLoadWithoutTheHookExplainsSetupInOneLine(t *testing.T) {
	root := newRootCommand()
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"env", "load"})
	err := root.Execute()
	if err == nil || strings.Contains(err.Error(), "\n") || !strings.Contains(err.Error(), "pb env hook --install") || !strings.Contains(err.Error(), "pb env run --") {
		t.Fatalf("err = %v", err)
	}
}
