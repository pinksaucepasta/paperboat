package daemoncmd

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestServiceCommandRegistration(t *testing.T) {
	root := NewCommand()

	for _, sub := range []string{"install", "uninstall", "start", "stop", "restart", "status"} {
		cmd, _, err := root.Find([]string{"service", sub})
		if err != nil || cmd == nil || cmd.Name() != sub {
			t.Fatalf("expected 'service %s' command to be registered: %v", sub, err)
		}
	}

	runCmd, _, err := root.Find([]string{"run"})
	if err != nil || runCmd == nil || runCmd.Name() != "run" {
		t.Fatalf("expected 'run' command to be registered: %v", err)
	}

	var buf bytes.Buffer
	root.SetArgs([]string{"service", "--help"})
	root.SetOut(&buf)
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("unexpected error running 'service --help': %v", err)
	}
	if !strings.Contains(buf.String(), "Manage the Paperboat background daemon service") {
		t.Fatalf("expected service help text, got: %s", buf.String())
	}
}

func TestRPCCommandsRegistration(t *testing.T) {
	commands := map[string]*cobra.Command{
		"resolve": ResolveCommand(),
		"tag":     TagCommand(),
		"approve": ApproveCommand(),
	}
	for name, cmd := range commands {
		if cmd == nil || cmd.Name() != name && !strings.HasPrefix(cmd.Use, name) {
			t.Fatalf("expected command %s to be valid", name)
		}
	}
}

func TestServiceInstallRejectsRelativeConfiguration(t *testing.T) {
	command := serviceInstallCommand()
	command.SetArgs([]string{"--config", "relative.json"})
	if err := command.ExecuteContext(t.Context()); err == nil || !strings.Contains(err.Error(), "must be absolute") {
		t.Fatalf("relative service config error = %v", err)
	}
}

func TestRunCommandDelegatesToLocalDaemonAndPreservesStartupFailure(t *testing.T) {
	command := NewCommand()
	invalid := filepath.Join(t.TempDir(), "invalid.json")
	if err := os.WriteFile(invalid, []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	command.SetArgs([]string{"run", "--config", invalid, "--server", "https://api.example.test"})
	if err := command.ExecuteContext(t.Context()); err == nil || !strings.Contains(err.Error(), "parse config "+invalid) {
		t.Fatalf("daemon configuration error = %v", err)
	}
}

func TestServiceCommandsRejectCompetingOwnerFlags(t *testing.T) {
	for _, action := range []string{"install", "uninstall", "start", "stop", "restart", "status"} {
		command := serviceCommand()
		command.SetArgs([]string{action, "--service-name", "other"})
		if err := command.ExecuteContext(t.Context()); err == nil {
			t.Fatalf("%s accepted arbitrary service owner", action)
		}
	}
}
