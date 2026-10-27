//go:build linux || darwin

package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// While an approved update activates the updater restarts and its socket is gone; `pb update status` must say so and
// give the retry step, not the generic "could not finish the command" (dadape, 2026-10-27).
func TestUpdateStatusNamesTheRestartingUpdater(t *testing.T) {
	previous := updateControlSocketForCommand
	updateControlSocketForCommand = func() (string, error) { return filepath.Join(t.TempDir(), "missing.sock"), nil }
	t.Cleanup(func() { updateControlSocketForCommand = previous })
	command := &cobra.Command{Use: "status"}
	command.Flags().Bool("json", false, "")
	command.SetContext(context.Background())

	err := actionUpdateStatus(command, nil)
	if err == nil {
		t.Fatal("status succeeded without an updater")
	}
	failure := classifyCommandFailure(err)
	message := commandFailureMessage(err, failure)
	if failure.kind != commandRejected || !strings.Contains(message, "restarts while an approved update activates") || !strings.Contains(message, "pb update status") {
		t.Fatalf("kind=%v message=%q", failure.kind, message)
	}
	if result := classifyCLIJSONError(err); result.Code != "updater_restarting" || !result.Retryable || result.StateChanged != false {
		t.Fatalf("json = %+v", result)
	}
}
