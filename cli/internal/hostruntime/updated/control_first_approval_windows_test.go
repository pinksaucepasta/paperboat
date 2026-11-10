//go:build windows

package updated

import "testing"

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
