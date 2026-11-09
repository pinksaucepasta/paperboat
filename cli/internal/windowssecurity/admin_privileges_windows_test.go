//go:build windows

package windowssecurity

import (
	"os/exec"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

// An elevated console starts with these privileges disabled; the installer needs them enabled (Windows Sandbox,
// 2026-11-10: first install and reset failed on LocalSystem-owned directories).
func TestEnableAdministrativeFilePrivilegesEnablesWhatTheTokenHolds(t *testing.T) {
	before := whoamiPrivileges(t)
	if !strings.Contains(before, "SeRestorePrivilege") {
		t.Skip("the process token does not hold SeRestorePrivilege (not elevated)")
	}
	// Start from an elevated console's defaults.
	var token windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_ADJUST_PRIVILEGES|windows.TOKEN_QUERY, &token); err != nil {
		t.Fatal(err)
	}
	defer token.Close()
	for _, name := range adminFilePrivileges {
		encoded, _ := windows.UTF16PtrFromString(name)
		var luid windows.LUID
		if err := windows.LookupPrivilegeValue(nil, encoded, &luid); err != nil {
			t.Fatal(err)
		}
		privileges := windows.Tokenprivileges{PrivilegeCount: 1}
		privileges.Privileges[0] = windows.LUIDAndAttributes{Luid: luid}
		if err := windows.AdjustTokenPrivileges(token, false, &privileges, 0, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := EnableAdministrativeFilePrivileges(); err != nil {
		t.Fatal(err)
	}
	after := whoamiPrivileges(t)
	for _, name := range adminFilePrivileges {
		if !privilegeEnabled(after, name) {
			t.Fatalf("%s is not enabled after EnableAdministrativeFilePrivileges:\n%s", name, after)
		}
	}
}

// whoamiPrivileges lists the privileges a child inherits from this process's token.
func whoamiPrivileges(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("whoami.exe", "/priv").CombinedOutput()
	if err != nil {
		t.Fatalf("whoami /priv: %v", err)
	}
	return string(out)
}

func privilegeEnabled(listing, name string) bool {
	for _, line := range strings.Split(listing, "\n") {
		if strings.HasPrefix(line, name+" ") {
			return strings.HasSuffix(strings.TrimSpace(line), "Enabled")
		}
	}
	return false
}
