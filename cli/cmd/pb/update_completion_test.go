//go:build linux || darwin

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/updated"
	"github.com/spf13/cobra"
)

// F14-08 device finding: an offline bundle installed (version changed, doctor 0)
// but the command exited 1 with the generic text, inviting a repeat of an
// update that had already landed.
func TestUpdateFromReportsFailuresAfterTheInstallLanded(t *testing.T) {
	previous := installUpdateSourceForCommand
	t.Cleanup(func() { installUpdateSourceForCommand = previous })
	directory := t.TempDir()
	notAProgram := filepath.Join(directory, "pb")
	if err := os.WriteFile(notAProgram, []byte("not an executable"), 0o700); err != nil {
		t.Fatal(err)
	}
	installUpdateSourceForCommand = func(context.Context, string, installsource.Source, string, bool, string) (string, error) {
		return notAProgram, nil
	}
	bundle := filepath.Join(directory, "release.pb-update")
	if err := os.WriteFile(bundle, []byte("bundle"), 0o600); err != nil {
		t.Fatal(err)
	}
	command := &cobra.Command{Use: "update"}
	command.Flags().Bool("json", true, "")
	err := installStagedUpdateSource(command, t.Context(), bundle, false)
	failure := classifyCommandFailure(err)
	message := commandFailureMessage(err, failure)
	if !strings.Contains(message, "was updated") || !strings.Contains(message, "pb doctor") {
		t.Fatalf("message=%q err=%v", message, err)
	}
	encoded := classifyCLIJSONFailure(err, failure)
	raw, _ := json.Marshal(encoded)
	if encoded.Code != "update_installed_unverified" || encoded.StateChanged != true || !strings.Contains(string(raw), "do not repeat") {
		t.Fatalf("json=%s", raw)
	}
}

func TestUpdateFromSaysWhenItIsAlreadyOnThatVersion(t *testing.T) {
	actual := installsource.Source{Version: "2026.10.20.12", Distribution: installsource.Official}
	for _, test := range []struct{ previous, want string }{
		{"2026.10.20.12", "Paperboat is already on 2026.10.20.12"},
		{"2026.10.20.8", "Updated Paperboat from 2026.10.20.8 to 2026.10.20.12."},
	} {
		var out bytes.Buffer
		command := &cobra.Command{Use: "update"}
		command.Flags().Bool("json", false, "")
		command.SetOut(&out)
		if err := writeUpdateFromResult(command, test.previous, "/home/u/.local/bin/pb", actual); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), test.want) {
			t.Fatalf("previous %s: output %q lacks %q", test.previous, out.String(), test.want)
		}
	}
	var out bytes.Buffer
	command := &cobra.Command{Use: "update"}
	command.Flags().Bool("json", true, "")
	command.SetOut(&out)
	if err := writeUpdateFromResult(command, actual.Version, "/x/pb", actual); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"changed":false`) {
		t.Fatalf("json %q does not say nothing changed", out.String())
	}
}

// F14-02: an update refused because SSH connections are open says how many and that --force ends
// them, in text and in JSON, and it is a retryable refusal that changed nothing.
func TestUpdateRefusedForOpenConnectionsNamesThemAndForce(t *testing.T) {
	refusal := &updated.ControlError{Code: "active_terminal_sessions", Message: "Paperboat is busy: 2 open SSH connections. Wait, close them, or use --force."}
	err := updateControlFailure("install approved update", refusal)
	failure := classifyCommandFailure(err)
	if failure.kind != commandRejected || commandFailureMessage(err, failure) != refusal.Message {
		t.Fatalf("kind=%v message=%q", failure.kind, commandFailureMessage(err, failure))
	}
	result := classifyCLIJSONError(err)
	if result.StateChanged != false || !result.Retryable || result.Message != refusal.Message || !strings.Contains(result.Recovery, "--force") {
		t.Fatalf("json = %+v", result)
	}
	// Terminal sessions keep their own, older wording.
	terminal := updateControlFailure("install approved update", &updated.ControlError{Code: "active_terminal_sessions", Message: "update is waiting for running or detached terminal sessions to finish"})
	if strings.Contains(commandFailureMessage(terminal, classifyCommandFailure(terminal)), "open SSH") {
		t.Fatal("terminal-session refusal gained ssh wording")
	}
}
