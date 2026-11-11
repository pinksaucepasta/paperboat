//go:build windows

package service

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/windowssecurity"
	"golang.org/x/sys/windows"
)

func requireElevatedDeclarationFixture(t *testing.T) *windows.SID {
	t.Helper()
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
	sid, err := user.User.Sid.Copy()
	if err != nil {
		t.Fatal(err)
	}
	return sid
}

func wellKnownSID(t *testing.T, kind windows.WELL_KNOWN_SID_TYPE) *windows.SID {
	t.Helper()
	sid, err := windows.CreateWellKnownSid(kind)
	if err != nil {
		t.Fatal(err)
	}
	return sid
}

// plantDirectory creates path with the given owner and protected DACL, as another account (or an older state) would.
func plantDirectory(t *testing.T, path string, owner *windows.SID, sddl string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
	descriptor, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err := windowssecurity.WithRestorePrivilege(func() error {
		return windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, owner, nil, dacl, nil)
	}); err != nil {
		t.Fatal(err)
	}
}

func requireCustody(t *testing.T, path string, owner *windows.SID, sddl string) {
	t.Helper()
	if !windowssecurity.OwnerMatchesSID(path, owner) || !windowssecurity.ProtectedDACLMatches(path, sddl) {
		t.Fatalf("%s: want owner %s and %s", path, owner, sddl)
	}
}

func TestNativeWindowsDeclarationPublishedWithSystemOwner(t *testing.T) {
	requireElevatedDeclarationFixture(t)
	// The parent is missing too: it is created protected, like C:\ProgramData\Paperboat on a fresh machine.
	root := filepath.Join(t.TempDir(), "Paperboat", "services")
	if err := prepareAtomicDirectory(root); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "owned.json")
	body := []byte("task-owned native declaration")
	if err := writeServiceDefinition(path, body, 0600); err != nil {
		t.Fatal(err)
	}
	system := wellKnownSID(t, windows.WinLocalSystemSid)
	requireCustody(t, path, system, "D:P(A;;FA;;;SY)(A;;FA;;;BA)")
	// Pin cleanup refuses a declaration directory an administrator owns (winpool-4, 2026-11-11: old pins never pruned).
	requireCustody(t, root, system, declarationDirectoryDACL)
	found, err := os.ReadFile(path)
	if err != nil || string(found) != string(body) {
		t.Fatal("declaration bytes changed", err)
	}
}

func TestDeclarationDirectoryOwnedByAdministratorsIsAdoptedAsSystem(t *testing.T) {
	requireElevatedDeclarationFixture(t)
	parent := filepath.Join(t.TempDir(), "Paperboat")
	plantDirectory(t, parent, wellKnownSID(t, windows.WinLocalSystemSid), "D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;0x1200a9;;;BU)")
	root := filepath.Join(parent, "services")
	plantDirectory(t, root, wellKnownSID(t, windows.WinBuiltinAdministratorsSid), declarationDirectoryDACL)
	if err := prepareAtomicDirectory(root); err != nil {
		t.Fatal(err)
	}
	requireCustody(t, root, wellKnownSID(t, windows.WinLocalSystemSid), declarationDirectoryDACL)
}

func TestPlantedDeclarationDirectoryIsRefusedUnchanged(t *testing.T) {
	user := requireElevatedDeclarationFixture(t)
	administrators := wellKnownSID(t, windows.WinBuiltinAdministratorsSid)
	for _, test := range []struct {
		name               string
		owner, parentOwner *windows.SID
		dacl, parentDACL   string
	}{
		{"owned by a non-admin account", user, administrators, declarationDirectoryDACL, declarationDirectoryDACL},
		{"extra Users write entry", administrators, administrators, declarationDirectoryDACL + "(A;OICI;FA;;;BU)", declarationDirectoryDACL},
		{"parent writable by Users", administrators, administrators, declarationDirectoryDACL, declarationDirectoryDACL + "(A;CI;0x6;;;BU)"},
		{"parent owned by a non-admin account", administrators, user, declarationDirectoryDACL, declarationDirectoryDACL},
	} {
		t.Run(test.name, func(t *testing.T) {
			parent := filepath.Join(t.TempDir(), "Paperboat")
			plantDirectory(t, parent, test.parentOwner, test.parentDACL)
			root := filepath.Join(parent, "services")
			plantDirectory(t, root, test.owner, test.dacl)
			if err := prepareAtomicDirectory(root); !errors.Is(err, ErrInvalidDefinition) {
				t.Fatalf("prepareAtomicDirectory = %v, want a refusal", err)
			}
			requireCustody(t, root, test.owner, test.dacl)
			requireCustody(t, parent, test.parentOwner, test.parentDACL)
		})
	}
}

func TestDeclarationDirectoryJunctionIsRefused(t *testing.T) {
	requireElevatedDeclarationFixture(t)
	parent := filepath.Join(t.TempDir(), "Paperboat")
	plantDirectory(t, parent, wellKnownSID(t, windows.WinBuiltinAdministratorsSid), declarationDirectoryDACL)
	target := filepath.Join(t.TempDir(), "elsewhere")
	administrators := wellKnownSID(t, windows.WinBuiltinAdministratorsSid)
	plantDirectory(t, target, administrators, declarationDirectoryDACL)
	root := filepath.Join(parent, "services")
	if output, err := exec.Command("cmd.exe", "/c", "mklink", "/J", root, target).CombinedOutput(); err != nil {
		t.Fatalf("create junction: %v: %s", err, output)
	}
	if err := prepareAtomicDirectory(root); !errors.Is(err, ErrInvalidDefinition) {
		t.Fatalf("prepareAtomicDirectory = %v, want a refusal", err)
	}
	requireCustody(t, target, administrators, declarationDirectoryDACL)
}

func TestDeclarationDirectoryCustodyReadBackMismatchFails(t *testing.T) {
	requireElevatedDeclarationFixture(t)
	root := filepath.Join(t.TempDir(), "services")
	plantDirectory(t, root, wellKnownSID(t, windows.WinBuiltinAdministratorsSid), declarationDirectoryDACL)
	handle, err := openProtectedWindowsDirectory(root)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(handle)
	if err := verifyDeclarationDirectoryCustody(handle, root); !errors.Is(err, ErrInvalidDefinition) {
		t.Fatalf("verify an Administrators-owned directory = %v, want a mismatch", err)
	}
}
