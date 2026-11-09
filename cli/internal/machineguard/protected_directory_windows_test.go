//go:build windows

package machineguard

import (
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

// A user's elevated console holds SeRestorePrivilege disabled. Creating the LocalSystem-owned guard directories
// then failed with "This security ID may not be assigned as the owner of this object", so the first pb install on a
// clean Windows machine failed (Windows Sandbox, 2026-11-10). An SSH session's token has the privilege enabled,
// which is why the test device never showed it.
func TestProtectedDirectoryIsCreatedFromATokenWithRestoreDisabled(t *testing.T) {
	setRestorePrivilege(t, false)
	t.Cleanup(func() { setRestorePrivilege(t, true) })
	root := t.TempDir()
	t.Setenv("ProgramData", root)
	path := filepath.Join(root, "Paperboat", "MachineGuard")
	if err := protectedDirectory(path, 0o700); err != nil {
		t.Fatalf("create the LocalSystem-owned directory: %v", err)
	}
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		t.Fatal(err)
	}
	if !protectedMachineDirectory(path, system) || !protectedMachineAncestor(filepath.Dir(path), system) {
		t.Fatal("created directories are not owned and protected by LocalSystem")
	}
}

// setRestorePrivilege enables or disables SeRestorePrivilege and SeBackupPrivilege in the process token, the two an
// elevated console holds disabled; a token that does not hold them (not elevated) skips the test.
func setRestorePrivilege(t *testing.T, enabled bool) {
	t.Helper()
	var token windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_ADJUST_PRIVILEGES|windows.TOKEN_QUERY, &token); err != nil {
		t.Fatal(err)
	}
	defer token.Close()
	for _, privilege := range []string{"SeRestorePrivilege", "SeBackupPrivilege"} {
		name, _ := windows.UTF16PtrFromString(privilege)
		var luid windows.LUID
		if err := windows.LookupPrivilegeValue(nil, name, &luid); err != nil {
			t.Fatal(err)
		}
		privileges := windows.Tokenprivileges{PrivilegeCount: 1}
		privileges.Privileges[0] = windows.LUIDAndAttributes{Luid: luid}
		if enabled {
			privileges.Privileges[0].Attributes = windows.SE_PRIVILEGE_ENABLED
		}
		if err := windows.AdjustTokenPrivileges(token, false, &privileges, 0, nil, nil); err != nil {
			t.Fatal(err)
		}
		if windows.GetLastError() == windows.ERROR_NOT_ALL_ASSIGNED {
			t.Skip("the process token does not hold " + privilege + " (not elevated)")
		}
	}
}
