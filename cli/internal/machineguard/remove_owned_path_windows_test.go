//go:build windows

package machineguard

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

// The guard's state is readable by LocalSystem only. From a token with the backup and restore privileges disabled
// (an administrator's elevated console) plain removal is refused; removeOwnedPath must still remove the tree
// (Windows Sandbox, 2026-11-10: pb reset failed with "openfdat ...\MachineGuard\diagnostics: Access is denied").
func TestRemoveOwnedPathRemovesASystemOnlyTreeWithPrivilegesDisabled(t *testing.T) {
	setRestorePrivilege(t, false) // skips when the token does not hold the privilege
	t.Cleanup(func() { setRestorePrivilege(t, true) })
	root := filepath.Join(t.TempDir(), "MachineGuard")
	inner := filepath.Join(root, "diagnostics")
	if err := os.MkdirAll(inner, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(inner, "events.log"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	descriptor, err := windows.SecurityDescriptorFromString("D:P(A;OICI;GA;;;SY)")
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(inner, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.ReadDir(inner); err == nil {
		t.Fatal("fixture: a LocalSystem-only directory is readable with the privileges disabled")
	}
	if err := removeOwnedPath(root); err != nil {
		t.Fatalf("remove a LocalSystem-only tree: %v", err)
	}
	if _, err := os.Lstat(root); !os.IsNotExist(err) {
		t.Fatalf("the tree is still there: %v", err)
	}
}
