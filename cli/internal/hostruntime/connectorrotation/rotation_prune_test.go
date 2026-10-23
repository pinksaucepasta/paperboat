package connectorrotation

import (
	"fmt"
	"testing"
	"time"
)

// A connector keeps rotating after 64 rotations: finished ones (old credential revoked) are pruned
// oldest first, the record being saved and unfinished rotations are kept (Lane X audit H3).
func TestTheRotationJournalPrunesFinishedRotations(t *testing.T) {
	start := time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC)
	records := map[string]Record{}
	for i := 0; i < maxJournalRecords; i++ {
		id := fmt.Sprintf("op_%03d", i)
		records[id] = Record{OperationID: id, Phase: PhaseRevoked, UpdatedAt: start.Add(time.Duration(i) * time.Minute)}
	}
	records["op_inflight"] = Record{OperationID: "op_inflight", Phase: PhaseInstalled, UpdatedAt: start}
	records["op_new"] = Record{OperationID: "op_new", Phase: PhaseChallenged, UpdatedAt: start.Add(time.Hour)}
	pruneFinishedRecords(records, "op_new")
	if len(records) != maxJournalRecords {
		t.Fatalf("records=%d, want %d", len(records), maxJournalRecords)
	}
	for _, kept := range []string{"op_new", "op_inflight", fmt.Sprintf("op_%03d", maxJournalRecords-1)} {
		if _, ok := records[kept]; !ok {
			t.Fatalf("%s was pruned", kept)
		}
	}
	for _, dropped := range []string{"op_000", "op_001"} {
		if _, ok := records[dropped]; ok {
			t.Fatalf("oldest finished %s was kept", dropped)
		}
	}
	unfinished := map[string]Record{}
	for i := 0; i <= maxJournalRecords; i++ {
		id := fmt.Sprintf("op_%03d", i)
		unfinished[id] = Record{OperationID: id, Phase: PhaseReady}
	}
	pruneFinishedRecords(unfinished, "op_000")
	if len(unfinished) != maxJournalRecords+1 {
		t.Fatal("an unfinished rotation was pruned")
	}
}
