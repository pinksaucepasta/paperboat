package nodelifecycle

import (
	"encoding/json"
	"os"
	"testing"
)

// node-state.json as 2026.11.16.2 wrote it (testdata from that release's diskState) decodes as Start reads it.
func TestLoadsNodeStateWrittenByPreviousRelease(t *testing.T) {
	raw, err := os.ReadFile("testdata/node-state-2026.11.16.2.json")
	if err != nil {
		t.Fatal(err)
	}
	var s diskState
	if err := json.Unmarshal(raw, &s); err != nil || s.NodeID != "relay_fixture" || s.Generation != 7 || s.PendingEpoch != "epoch_fixture" {
		t.Fatalf("previous release's node state = %+v, %v", s, err)
	}
}
