//go:build windows

package bootstrap

import "testing"

// Clean Windows Sandbox, paired official-flagged install: `pb update --from <build>` failed at install_recovery with
// "Access is denied." The updater service (LocalSystem) had secured its signed-metadata directory for LocalSystem
// alone, and the elevated installer, an administrator, checks the installed official version in that directory.
func TestTheUpdatersMetadataDirectoryAdmitsTheElevatedInstaller(t *testing.T) {
	if got := artifactDirectorySDDL("S-1-5-18"); got != "D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)" {
		t.Fatalf("LocalSystem's directory: %s", got)
	}
	// An account's own state stays its own: no other account, no Administrators entry.
	if got := artifactDirectorySDDL("S-1-5-21-1-2-3-1001"); got != "D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;S-1-5-21-1-2-3-1001)" {
		t.Fatalf("an account's directory: %s", got)
	}
}
