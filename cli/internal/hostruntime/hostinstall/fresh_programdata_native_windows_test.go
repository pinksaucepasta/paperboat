//go:build windows

package hostinstall

import (
	"path/filepath"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/windowssecurity"
	"golang.org/x/sys/windows"
)

// A fresh install creates C:\ProgramData\Paperboat and its folders itself. Hosted runners give an elevated token
// the user as default owner, so a plain create made folders the install then refused as planted (34b8f5d4e).
func TestFreshInstallFoldersAreCreatedWithAdministratorCustody(t *testing.T) {
	token, err := windows.OpenCurrentProcessToken()
	if err != nil {
		t.Fatal(err)
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	if !token.IsElevated() || user.User.Sid.String() == "S-1-5-18" {
		t.Skip("elevated enrolled administrator fixture required")
	}
	sid := user.User.Sid.String()
	previousRoot := windowsProgramDataRoot
	windowsProgramDataRoot = filepath.Join(t.TempDir(), "Paperboat")
	t.Cleanup(func() { windowsProgramDataRoot = previousRoot })
	instance, err := WindowsInstanceForSID(sid)
	if err != nil {
		t.Fatal(err)
	}
	shared := []string{windowsProgramDataRoot, filepath.Join(windowsProgramDataRoot, "users")}
	// The binary and release folders live under Program Files; a missing parent chain is created as well.
	instanceRoot := filepath.Join(t.TempDir(), "Paperboat", "users", instance)
	owned := []string{filepath.Join(instanceRoot, "bin"), filepath.Join(instanceRoot, "releases")}
	// Reproduce the hosted-runner token: the user, not Administrators, is the default owner of new objects.
	if err := windowssecurity.WithRestorePrivilegeAndOwner(user.User.Sid, func() error {
		for _, path := range shared {
			if err := ensureWindowsExecutableDirectory(path, ""); err != nil {
				return err
			}
		}
		for _, path := range owned {
			if err := ensureWindowsExecutableDirectory(path, sid); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	owner, err := windowsRuntimeTrustedOwner()
	if err != nil {
		t.Fatal(err)
	}
	for reader, paths := range map[string][]string{"BU": shared, sid: owned} {
		dacl := "D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;0x1200a9;;;" + reader + ")"
		for _, path := range paths {
			if !windowssecurity.OwnerMatchesSID(path, owner) || !windowssecurity.ProtectedDACLMatches(path, dacl) {
				t.Fatalf("%s: want owner %s and %s", path, owner, dacl)
			}
		}
	}
}
