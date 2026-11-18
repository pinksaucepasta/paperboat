package envinject

import (
	"os"
	"path/filepath"
	"testing"
)

// The MAC'd ENV layer state 2026.11.16.2 wrote (testdata from that release's writeLayerState) reads after an update.
func TestReadsLayerStateWrittenByPreviousRelease(t *testing.T) {
	raw, err := os.ReadFile("testdata/2026.11.16.2-layer-state.json")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "floor.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	var state layerHighWater
	if missing, err := readLayerState(path, key, &state); err != nil || missing || state.AccountID != "account_01" || state.InstallationGeneration != 3 {
		t.Fatalf("previous release's layer state = %+v, missing=%v, %v", state, missing, err)
	}
}
