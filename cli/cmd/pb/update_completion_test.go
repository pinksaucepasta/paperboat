//go:build linux || darwin

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostinstall"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/updated"
	"github.com/pinksaucepasta/paperboat/internal/liveconnections"
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
	installUpdateSourceForCommand = func(context.Context, string, installsource.Source, string, bool, string, string) (string, error) {
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

// F14-03: an update that cannot be staged because the disk is full says so and that nothing changed,
// instead of telling the user to approve administrator access.
func TestUpdateOnAFullDiskNamesTheDiskAndNothingChanged(t *testing.T) {
	err := fmt.Errorf("install: %w", hostinstall.ErrInsufficientSpace)
	failure := classifyCommandFailure(err)
	message := commandFailureMessage(err, failure)
	if failure.kind != commandRejected || !strings.Contains(message, "not enough free disk space") || !strings.Contains(message, "Nothing was changed") || strings.Contains(message, "administrator") {
		t.Fatalf("kind=%v message=%q", failure.kind, message)
	}
	result := classifyCLIJSONError(err)
	if result.Code != "insufficient_disk_space" || result.StateChanged != false || result.Retryable || !strings.Contains(result.Recovery, "Free disk space") {
		t.Fatalf("json = %+v", result)
	}
}

// F14-02: `pb update --from` ends open SSH connections too, so it refuses unless forced, and installs
// when none are open or the daemon cannot say.
func TestUpdateFromRefusesWhileSSHConnectionsAreOpen(t *testing.T) {
	previousInstall, previousOpen := installUpdateSourceForCommand, openConnectionsFromDaemon
	t.Cleanup(func() { installUpdateSourceForCommand, openConnectionsFromDaemon = previousInstall, previousOpen })
	installs := 0
	installUpdateSourceForCommand = func(context.Context, string, installsource.Source, string, bool, string, string) (string, error) {
		installs++
		return "", errors.New("stop after the install was attempted")
	}
	bundle := filepath.Join(t.TempDir(), "release.pb-update")
	if err := os.WriteFile(bundle, []byte("bundle"), 0o600); err != nil {
		t.Fatal(err)
	}
	command := &cobra.Command{Use: "update"}
	command.Flags().Bool("json", false, "")

	openConnectionsFromDaemon = func(context.Context) (liveconnections.Counts, error) {
		return liveconnections.Counts{SSH: 2}, nil
	}
	err := installStagedUpdateSource(command, t.Context(), bundle, false)
	failure := classifyCommandFailure(err)
	if installs != 0 || failure.kind != commandRejected || commandFailureMessage(err, failure) != "Paperboat is busy: 2 open SSH connections. Wait, close them, or use --force." {
		t.Fatalf("installs=%d kind=%v message=%q", installs, failure.kind, commandFailureMessage(err, failure))
	}
	if err = installStagedUpdateSource(command, t.Context(), bundle, true); installs != 1 {
		t.Fatalf("--force did not proceed: installs=%d err=%v", installs, err)
	}
	openConnectionsFromDaemon = func(context.Context) (liveconnections.Counts, error) { return liveconnections.Counts{}, nil }
	_ = installStagedUpdateSource(command, t.Context(), bundle, false)
	openConnectionsFromDaemon = func(context.Context) (liveconnections.Counts, error) {
		return liveconnections.Counts{SSH: 5}, errors.New("daemon cannot answer")
	}
	_ = installStagedUpdateSource(command, t.Context(), bundle, false)
	if installs != 3 {
		t.Fatalf("an idle daemon or one that cannot answer blocked the install: installs=%d", installs)
	}
}

// F14-03: an update whose new version's daemon cannot start is rolled back and says so plainly.
func TestUpdateWhoseDaemonCannotStartIsReportedAsRolledBack(t *testing.T) {
	err := fmt.Errorf("install: %w", hostinstall.ErrUpdateNotReady)
	failure := classifyCommandFailure(err)
	message := commandFailureMessage(err, failure)
	if failure.kind != commandRejected || !strings.Contains(message, "previous version was restored") || !strings.Contains(message, "nothing was changed") || strings.Contains(message, "administrator") {
		t.Fatalf("kind=%v message=%q", failure.kind, message)
	}
	result := classifyCLIJSONError(err)
	if result.Code != "update_rolled_back" || result.StateChanged != false || !result.Retryable {
		t.Fatalf("json = %+v", result)
	}
}
