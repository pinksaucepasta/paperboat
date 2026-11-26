//go:build windows

package updated

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/workerupdate"
)

// After a rolled-back update an explicit `pb update` retries the release; automatic updates keep refusing it,
// with release_quarantined (unix: e2e9ddada, 2a5f07511).
func TestWindowsQuarantinedReleaseRetriesOnlyManually(t *testing.T) {
	root := t.TempDir()
	if err := windowsReleaseQuarantined(root, false); err != nil {
		t.Fatalf("no marker: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, ".quarantined"), []byte("health\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := windowsReleaseQuarantined(root, false)
	if !errors.Is(err, workerupdate.ErrQuarantined) || controlErrorCodeWindows(err) != "release_quarantined" {
		t.Fatalf("automatic: %v code=%s", err, controlErrorCodeWindows(err))
	}
	if err := windowsReleaseQuarantined(root, true); err != nil {
		t.Fatalf("manual retry refused: %v", err)
	}
}

// A scheduled check while an activation holds hostd's stability gate makes no release lookup (unix: ded7a1c00);
// a candidate still waiting for its first approval holds no gate, so checks go on.
func TestWindowsCheckReleaseSkipsLookupDuringActivation(t *testing.T) {
	journal := testWindowsActivationJournal()
	previous := loadWindowsActivationJournalForController
	t.Cleanup(func() { loadWindowsActivationJournalForController = previous })
	loadWindowsActivationJournalForController = func(WindowsConfig) (windowsActivationJournal, error) { return journal, nil }
	calls := 0
	c := &windowsController{activeVersion: journal.PreviousVersion, resolve: func(context.Context) (workerupdate.Release, bool, error) {
		calls++
		return workerupdate.Release{}, false, nil
	}}
	journal.Stage = windowsActivationServicesLive
	if got, err := c.checkRelease(context.Background()); err != nil || calls != 0 || got.Version != journal.PreviousVersion {
		t.Fatalf("during activation: %+v calls=%d err=%v", got, calls, err)
	}
	journal.Stage, journal.ApprovedCandidateID = windowsActivationAwaitingApproval, ""
	if _, err := c.checkRelease(context.Background()); err != nil || calls != 1 {
		t.Fatalf("awaiting first approval: calls=%d err=%v", calls, err)
	}
}
