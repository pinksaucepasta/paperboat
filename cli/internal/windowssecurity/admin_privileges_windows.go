//go:build windows

package windowssecurity

import (
	"errors"

	"golang.org/x/sys/windows"
)

// adminFilePrivileges are what the elevated installer needs to create, re-own and remove LocalSystem-owned
// files: an administrator's token holds them, but disabled.
var adminFilePrivileges = []string{"SeRestorePrivilege", "SeBackupPrivilege", "SeTakeOwnershipPrivilege", "SeSecurityPrivilege"}

// EnableAdministrativeFilePrivileges enables, for this process, the file-custody privileges its token holds.
// Only the short-lived elevated helpers call it (install, repair, reset, uninstall). Windows gives the same
// administrator different defaults by logon type: a network logon (SSH, where this installer was developed and
// tested) has every privilege enabled, an elevated console has them disabled, and the installer then failed on
// its first LocalSystem-owned object. A privilege the token does not hold is left alone (not an error).
func EnableAdministrativeFilePrivileges() error {
	var token windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_ADJUST_PRIVILEGES|windows.TOKEN_QUERY, &token); err != nil {
		return err
	}
	defer token.Close()
	var result error
	for _, name := range adminFilePrivileges {
		encoded, err := windows.UTF16PtrFromString(name)
		if err != nil {
			return err
		}
		var luid windows.LUID
		if err := windows.LookupPrivilegeValue(nil, encoded, &luid); err != nil {
			result = errors.Join(result, err)
			continue
		}
		privileges := windows.Tokenprivileges{PrivilegeCount: 1}
		privileges.Privileges[0] = windows.LUIDAndAttributes{Luid: luid, Attributes: windows.SE_PRIVILEGE_ENABLED}
		if err := windows.AdjustTokenPrivileges(token, false, &privileges, 0, nil, nil); err != nil {
			result = errors.Join(result, err)
		}
	}
	return result
}
