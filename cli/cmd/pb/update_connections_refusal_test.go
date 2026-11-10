package main

import (
	"context"
	"errors"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/liveconnections"
)

// F14-02 on Windows: the signed `pb update` refuses too (the Windows updater service cannot ask for the user's
// connections, so the CLI does), --force and an idle or unreachable daemon proceed, and Unix leaves it to its service.
func TestSignedUpdateRefusesOnWindowsWhileSSHConnectionsAreOpen(t *testing.T) {
	previous := openConnectionsFromDaemon
	t.Cleanup(func() { openConnectionsFromDaemon = previous })
	open := func(counts liveconnections.Counts, err error) {
		openConnectionsFromDaemon = func(context.Context) (liveconnections.Counts, error) { return counts, err }
	}
	open(liveconnections.Counts{SSH: 1, Forwards: 1}, nil)
	err := refuseSignedUpdateWhileConnectionsOpen(t.Context(), "windows", false)
	failure := classifyCommandFailure(err)
	if failure.kind != commandRejected || commandFailureMessage(err, failure) != "Paperboat is busy: 1 open SSH connection and 1 open port forward. Wait, close them, or use --force." {
		t.Fatalf("kind=%v message=%q", failure.kind, commandFailureMessage(err, failure))
	}
	if err := refuseSignedUpdateWhileConnectionsOpen(t.Context(), "windows", true); err != nil {
		t.Fatalf("--force was refused: %v", err)
	}
	if err := refuseSignedUpdateWhileConnectionsOpen(t.Context(), "linux", false); err != nil {
		t.Fatalf("Unix refuses inside its updater service, not here: %v", err)
	}
	open(liveconnections.Counts{}, nil)
	if err := refuseSignedUpdateWhileConnectionsOpen(t.Context(), "windows", false); err != nil {
		t.Fatalf("an idle daemon blocked the update: %v", err)
	}
	open(liveconnections.Counts{SSH: 5}, errors.New("daemon cannot answer"))
	if err := refuseSignedUpdateWhileConnectionsOpen(t.Context(), "windows", false); err != nil {
		t.Fatalf("a daemon that cannot answer blocked the update: %v", err)
	}
}
