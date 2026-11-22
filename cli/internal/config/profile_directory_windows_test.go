//go:build windows

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/cli/internal/windowssecurity"
	"golang.org/x/sys/windows"
)

func TestEnsureProfileDirectoryProtectsCredentialNamespace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials", "profiles")
	if err := ensureProfileDirectory(path); err != nil {
		t.Fatal(err)
	}
	sddl, err := currentUserDirectorySDDL()
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		t.Fatal(err)
	}
	if !windowssecurity.ProtectedDACLMatches(path, descriptor.String()) {
		t.Fatalf("profile directory does not have the protected current-user DACL: %s", descriptor.String())
	}
	// Children created with the token default must inherit the owner-only ACEs: an unelevated administrator
	// network logon's default DACL excludes the user, so a non-inheriting parent leaves them Access denied.
	child := filepath.Join(path, "child")
	if err := os.Mkdir(child, 0o700); err != nil {
		t.Fatal(err)
	}
	inherited, err := windows.GetNamedSecurityInfo(child, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	if got := inherited.String(); !strings.Contains(got, "(A;OICIID;FA;;;SY)") || !strings.Contains(got, "(A;OICIID;FA;;;BA)") || strings.Count(got, "(A;OICIID;FA;;;") != 3 {
		t.Fatalf("child of the profile directory did not inherit the owner-only DACL: %s", got)
	}
}
