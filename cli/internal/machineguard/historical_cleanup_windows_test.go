//go:build windows

package machineguard

import (
	"os"
	"path/filepath"
	"testing"
)

// This fixture uses its own state, registry branch and exact certificate. It
// never installs/stops a guard.
func TestWindowsHistoricalCleanupIsScoped(t *testing.T) {
	if os.Getenv("PAPERBOAT_WINDOWS_HISTORICAL_CLEANUP") != "1" {
		t.Skip("explicit LocalSystem scoped cleanup acceptance")
	}
	root := t.TempDir()
	t.Setenv("ProgramData", root)
	state := filepath.Join(root, "Paperboat", "guard")
	directory := filepath.Join(state, "certificates", "removalfixture")
	if err := protectedDirectory(directory, 0700); err != nil {
		t.Fatal(err)
	}
	owned := createOwnedRootFixture(t, state, "removalfixture")
	t.Cleanup(func() {
		if err := removeWindowsRoot(owned); err != nil {
			t.Error(err)
		}
	})
	if err := addWindowsRootFixture(owned); err != nil {
		t.Fatal(err)
	}
	if present, err := windowsUninstallRootPresent(owned.certificate.Raw); err != nil || !present {
		t.Fatal("fixture root not installed", err)
	}
	roots, err := uninstallRoots(state)
	if err != nil || len(roots) != 1 {
		t.Fatal("owned state discovery", err)
	}
	if err := cleanupWindowsOwnedTrust(t.Context(), roots); err != nil {
		t.Fatal(err)
	}
	if present, err := windowsUninstallRootPresent(owned.certificate.Raw); err != nil || present {
		t.Fatal("owned trust survived cleanup", err)
	}
	if _, err := os.Stat(directory); !os.IsNotExist(err) {
		t.Fatal("owned key state survived", err)
	}
	if err := cleanupHistoricalTrust(t.Context(), Config{StateDir: state}); err != nil {
		t.Fatal("cleanup retry", err)
	}
}
