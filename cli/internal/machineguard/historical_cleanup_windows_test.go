//go:build windows

package machineguard

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows/registry"
)

// This fixture uses its own state, registry branch and exact certificate. It
// never installs/stops a guard or changes the real NRPT/hosts configuration.
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

	prior := nrptRoot
	nrptRoot = fmt.Sprintf(`SOFTWARE\PaperboatCleanupFixture-%d`, os.Getpid())
	t.Cleanup(func() {
		_ = registry.DeleteKey(registry.LOCAL_MACHINE, nrptRoot+`\Paperboat-Owned`)
		_ = registry.DeleteKey(registry.LOCAL_MACHINE, nrptRoot+`\Foreign`)
		_ = registry.DeleteKey(registry.LOCAL_MACHINE, nrptRoot)
		nrptRoot = prior
	})
	key, _, err := registry.CreateKey(registry.LOCAL_MACHINE, nrptRoot+`\Paperboat-Owned`, registry.ALL_ACCESS)
	if err != nil {
		t.Fatal(err)
	}
	if err := key.SetStringValue("PaperboatOwner", nrptOwner); err != nil {
		key.Close()
		t.Fatal(err)
	}
	key.Close()
	foreign, _, err := registry.CreateKey(registry.LOCAL_MACHINE, nrptRoot+`\Foreign`, registry.ALL_ACCESS)
	if err != nil {
		t.Fatal(err)
	}
	foreign.Close()
	hosts := filepath.Join(root, "hosts")
	outside := []byte("# foreign\r\n127.0.0.1 localhost\r\n")
	initial := append([]byte(hostsStart+"\n127.100.0.4 hp.pprbt\n"+hostsEnd+"\n"), outside...)
	if err := os.WriteFile(hosts, initial, 0644); err != nil {
		t.Fatal(err)
	}
	cfg := Config{StateDir: state, hostsPath: hosts}
	if err := cleanupHistoricalLocalNames(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	if key, err := registry.OpenKey(registry.LOCAL_MACHINE, nrptRoot+`\Paperboat-Owned`, registry.READ); err == nil {
		key.Close()
		t.Fatal("owned NRPT entry survived")
	}
	if key, err := registry.OpenKey(registry.LOCAL_MACHINE, nrptRoot+`\Foreign`, registry.READ); err != nil {
		t.Fatal("foreign registry entry removed", err)
	} else {
		key.Close()
	}
	data, err := os.ReadFile(hosts)
	if err != nil || !bytes.Equal(data, outside) {
		t.Fatal("foreign hosts bytes changed", err)
	}
	if err := cleanupHistoricalLocalNames(t.Context(), cfg); err != nil {
		t.Fatal("name cleanup retry", err)
	}
}
