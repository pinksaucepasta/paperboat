//go:build windows

package hostservice

import (
	"os"
	"path/filepath"
	"testing"
)

// The updater state 2026.11.16.2's Windows service wrote (its json.Marshal output, testdata) reads as healthy.
func TestReadsUpdaterStateWrittenByPreviousRelease(t *testing.T) {
	raw, err := os.ReadFile("testdata/2026.11.16.2-service-state.json")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "service-state.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	diagnostics := &windowsUpdateDiagnostics{statePath: filepath.Join(root, "service-state.json"), machineID: "machine_01", running: func() bool { return true }}
	if got := diagnostics.UpdateHealth(); got != "healthy" {
		t.Fatalf("previous release's updater state health = %q", got)
	}
}
