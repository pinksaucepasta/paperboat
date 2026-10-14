package main

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/prompt"
	"github.com/spf13/cobra"
)

func TestTunnelCreateMissingNamePromptsBeforeNetwork(t *testing.T) {
	oldTerminal, oldPrompt, oldClient := previewInteractiveTerminal, promptTunnelCreateName, tunnelClientForCommand
	defer func() {
		previewInteractiveTerminal, promptTunnelCreateName, tunnelClientForCommand = oldTerminal, oldPrompt, oldClient
	}()
	previewInteractiveTerminal = func(*cobra.Command) bool { return true }
	accepted := false
	sentinel := errors.New("network boundary reached")
	tunnelClientForCommand = func(*cobra.Command) (*api.Client, error) {
		if !accepted {
			t.Fatal("network started before accepting name")
		}
		return nil, sentinel
	}
	contextKey := struct{}{}
	ctx := context.WithValue(context.Background(), contextKey, "preferences-context")
	promptTunnelCreateName = func(options prompt.TextOptions) (string, error) {
		if options.Context.Value(contextKey) != "preferences-context" || options.Output == nil || options.Validate(".bad") == nil || options.Validate("my-app") != nil {
			t.Fatal("name prompt omitted context or validation")
		}
		accepted = true
		return "my-app", nil
	}
	command := tunnelCreateCommand()
	command.SetOut(io.Discard)
	command.SetErr(io.Discard)
	command.SetArgs([]string{"--port", "3000"})
	if err := command.ExecuteContext(ctx); !errors.Is(err, sentinel) {
		t.Fatalf("accepted name did not reach real workflow: %v", err)
	}
}
func TestTunnelCreateMissingNameCancelAndAutomation(t *testing.T) {
	oldTerminal, oldPrompt, oldClient := previewInteractiveTerminal, promptTunnelCreateName, tunnelClientForCommand
	defer func() {
		previewInteractiveTerminal, promptTunnelCreateName, tunnelClientForCommand = oldTerminal, oldPrompt, oldClient
	}()
	tunnelClientForCommand = func(*cobra.Command) (*api.Client, error) {
		t.Fatal("canceled/invalid invocation reached network")
		return nil, nil
	}
	previewInteractiveTerminal = func(*cobra.Command) bool { return true }
	calls := 0
	promptTunnelCreateName = func(prompt.TextOptions) (string, error) { calls++; return "", prompt.ErrCanceled }
	command := tunnelCreateCommand()
	command.SetOut(io.Discard)
	command.SetErr(io.Discard)
	command.SetArgs([]string{"--port", "3000"})
	if err := command.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("cancel was not clean: %v", err)
	}
	if calls != 1 {
		t.Fatal("missing interactive name prompt")
	}
	for _, test := range []struct {
		terminal bool
		args     []string
	}{{false, []string{"--port", "3000"}}, {true, []string{"--port", "3000", "--json"}}, {true, []string{"one", "two", "--port", "3000"}}} {
		previewInteractiveTerminal = func(*cobra.Command) bool { return test.terminal }
		command := tunnelCreateCommand()
		command.SetOut(io.Discard)
		command.SetErr(io.Discard)
		command.SetArgs(test.args)
		if err := command.ExecuteContext(context.Background()); !errors.Is(err, errUsage) {
			t.Fatalf("missing typed usage error: %v", err)
		}
	}
	if calls != 1 {
		t.Fatal("automation unexpectedly prompted")
	}
}
