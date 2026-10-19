//go:build linux || darwin || windows

package machineguard

import (
	"os"
	"testing"
)

// testBrowserDomain is the local access domain the tests configure.
const testBrowserDomain = "local.example.test"

// testStateDir is a guard state directory whose installation recorded testBrowserDomain.
func testStateDir(t *testing.T) string {
	t.Helper()
	state := t.TempDir()
	recordTestDefaultDomain(t, state)
	return state
}

func allowUnprivilegedDomainRecord(t *testing.T) {
	t.Helper()
	previous := validateBrowserDomainsFile
	validateBrowserDomainsFile = func(string, string, os.FileInfo) error { return nil }
	t.Cleanup(func() { validateBrowserDomainsFile = previous })
}

// recordTestDefaultDomain records testBrowserDomain as the installed default in state.
func recordTestDefaultDomain(t *testing.T, state string) {
	t.Helper()
	if os.Geteuid() != 0 {
		allowUnprivilegedDomainRecord(t)
	}
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := recordDefaultBrowserDomain(state, testBrowserDomain); err != nil {
		t.Fatal(err)
	}
}
