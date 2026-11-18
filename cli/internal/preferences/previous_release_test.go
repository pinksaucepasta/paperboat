package preferences

import (
	"os"
	"path/filepath"
	"testing"
)

// The preferences 2026.11.16.2 wrote (testdata from that release's Save) load after an update.
func TestLoadsPreferencesWrittenByPreviousRelease(t *testing.T) {
	raw, err := os.ReadFile("testdata/2026.11.16.2-preferences.json")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "pb.preferences.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err != nil {
		t.Fatalf("previous release's preferences: %v", err)
	}
}
