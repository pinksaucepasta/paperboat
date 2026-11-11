//go:build windows

package updated

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/workerupdate"
)

func TestWindowsFirstApprovalIsOnlyTheUnapprovedStagedCandidate(t *testing.T) {
	journal := testWindowsActivationJournal()
	journal.Stage, journal.ApprovedCandidateID = windowsActivationAwaitingApproval, ""
	id := journal.Candidate.ID
	if !windowsFirstApproval(journal, id) {
		t.Fatal("the exact candidate id of an unapproved staged candidate was not a first approval")
	}
	if windowsFirstApproval(journal, "") || windowsFirstApproval(journal, id+"x") {
		t.Fatal("an empty or other id approved the candidate")
	}
	journal.ApprovedCandidateID = id
	if windowsFirstApproval(journal, id) {
		t.Fatal("an already approved candidate was approved again from the fenced path")
	}
	journal.ApprovedCandidateID, journal.Stage = "", windowsActivationStaged
	if windowsFirstApproval(journal, id) {
		t.Fatal("a staged journal was treated as awaiting approval")
	}
}

// The first approval skips the activator ownership check because no activator can hold a journal that is still
// awaiting approval: nothing resumes it, whoever owns the transaction.
func TestWindowsAwaitingApprovalJournalIsNeverResumedByAnActivator(t *testing.T) {
	journal := testWindowsActivationJournal()
	journal.Stage, journal.ApprovedCandidateID = windowsActivationAwaitingApproval, ""
	for _, owned := range []bool{false, true} {
		if windowsActivationNeedsResume(journal, journal.PreviousVersion, owned) || windowsActivationNeedsResume(journal, journal.Version, owned) {
			t.Fatalf("an awaiting_approval journal needs resume (activator owns=%v)", owned)
		}
	}
}

// After a first approval the install path still verifies the staged file: bytes changed since staging are refused.
func TestWindowsFirstApprovalStillRefusesATamperedCandidateFile(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "candidate.exe")
	if err = os.WriteFile(path, body, 0o700); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(body)
	journal := testWindowsActivationJournal()
	journal.Stage, journal.ApprovedCandidateID, journal.Architecture = windowsActivationAwaitingApproval, "", runtime.GOARCH
	journal.Runtime = windowsActivationComponent{Path: path, SHA256: hex.EncodeToString(digest[:]), Length: int64(len(body))}
	journal.Candidate.SHA256, journal.Candidate.Length, journal.Candidate.Architecture = journal.Runtime.SHA256, journal.Runtime.Length, runtime.GOARCH
	if !windowsFirstApproval(journal, journal.Candidate.ID) {
		t.Fatal("the exact candidate id was not a first approval")
	}
	if err = verifyWindowsPreparedCandidate(context.Background(), journal); err != nil {
		t.Fatal(err)
	}
	body[len(body)-1] ^= 1
	if err = os.WriteFile(path, body, 0o700); err != nil {
		t.Fatal(err)
	}
	if err = verifyWindowsPreparedCandidate(context.Background(), journal); !errors.Is(err, workerupdate.ErrPreparedCandidate) {
		t.Fatalf("a tampered candidate passed verification after a first approval: %v", err)
	}
}

// The scheduler's check is download-then-install: while a candidate waits for its first approval, check and
// download must not be fenced (only an approved journal, which an activator may hold, fences them).
func TestWindowsCheckAndDownloadStayOpenWhileACandidateAwaitsApproval(t *testing.T) {
	journal := testWindowsActivationJournal()
	journal.Stage, journal.ApprovedCandidateID = windowsActivationAwaitingApproval, ""
	if !windowsAwaitsFirstApproval(journal) {
		t.Fatal("an unapproved staged candidate fenced check and download")
	}
	journal.ApprovedCandidateID = journal.Candidate.ID
	if windowsAwaitsFirstApproval(journal) {
		t.Fatal("an approved candidate no longer fences check and download")
	}
	for _, stage := range []windowsActivationStage{windowsActivationStaged, windowsActivationSwitching, windowsActivationCommitReady} {
		journal.Stage, journal.ApprovedCandidateID = stage, ""
		if windowsAwaitsFirstApproval(journal) {
			t.Fatalf("stage %q was treated as awaiting approval", string(stage))
		}
	}
}
