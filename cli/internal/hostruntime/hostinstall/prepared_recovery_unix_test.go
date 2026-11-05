//go:build linux || darwin

package hostinstall

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

// An update interrupted after the candidate staged its generation leaves a journal
// written for the candidate's units (its hostd generation, its carrier generation).
// Recovery used the previous installation's request, so it acted on the previous
// generation's live hostd (service: TestLifecycleRecoveryNeverAppliesAnotherGenerationsJournalEntry)
// and never removed the candidate's carrier generation. It rebuilds the journaled request.
func TestRecoveryRebuildsTheRequestTheJournalWasWrittenFor(t *testing.T) {
	previous := validRequest(t)
	paths := platformPaths(previous.UID)
	candidate := previous.Source
	candidate.SHA256 = strings.Repeat("c", 64)
	candidate.Version = "candidate"
	if err := candidate.Validate(); err != nil {
		t.Fatal(err)
	}
	native, err := json.Marshal(unixCarrierBaseline{Candidate: &candidate})
	if err != nil {
		t.Fatal(err)
	}
	got := recoveryRequest(previous, native, paths)
	if got.Source != candidate || got.Executable != filepath.Join(filepath.Dir(paths.workerRollback), "pins", candidate.SHA256, "pb") {
		t.Fatalf("recovery request %+v is not the journaled candidate's", got.Source)
	}
	got.Source, got.Executable = previous.Source, previous.Executable
	if got != previous {
		t.Fatal("recovery changed more than the candidate's source and pin")
	}
	for name, native := range map[string][]byte{"no journal": nil, "no candidate": []byte(`{"carrier_only":false}`), "unreadable": []byte("{")} {
		if got := recoveryRequest(previous, native, paths); got != previous {
			t.Fatalf("%s: recovery did not keep the previous request", name)
		}
	}
}
