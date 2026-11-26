package updated

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// winpool-8 2026-11-26: an update rolled back to .16, the next check replaced the journal and status
// said "Activation: complete".
func TestWindowsRolledBackUpdateIsReported(t *testing.T) {
	stateRoot := filepath.Join(t.TempDir(), "runtime")
	if err := os.MkdirAll(filepath.Dir(windowsActivationJournalPath(stateRoot)), 0o700); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 11, 26, 16, 0, 0, 0, time.UTC)
	journal := windowsActivationJournal{PreviousVersion: "2026.11.26.16", Version: "2026.11.26.17", Stage: windowsActivationRolledBack, Failure: "hostd did not become ready"}
	if err := recordWindowsUpdateOutcome(stateRoot, journal, at); err != nil {
		t.Fatal(err)
	}

	var response ControlResponse
	reportWindowsRolledBackUpdate(stateRoot, "2026.11.26.16", &response)
	state := response.Transaction
	if response.ActivationFailure != "activation_failed" || state.FailedVersion != "2026.11.26.17" || state.FailedStep != "activation" || state.FailedReason != "hostd did not become ready" || !state.UpdatedAt.Equal(at) {
		t.Fatalf("rolled-back update not reported: %+v", response)
	}

	var other ControlResponse
	reportWindowsRolledBackUpdate(stateRoot, "2026.11.26.18", &other)
	if other.ActivationFailure != "" || other.Transaction.FailedVersion != "" {
		t.Fatalf("a later version still reports the old rollback: %+v", other)
	}

	journal.Stage = windowsActivationCommitted
	if err := recordWindowsUpdateOutcome(stateRoot, journal, at); err != nil {
		t.Fatal(err)
	}
	var committed ControlResponse
	reportWindowsRolledBackUpdate(stateRoot, "2026.11.26.16", &committed)
	if committed.ActivationFailure != "" {
		t.Fatalf("a committed update still reports the rollback: %+v", committed)
	}
}
