//go:build windows

package configsync

import (
	"io/fs"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

// platformHasExecBits is false where files carry no POSIX executable bit (Windows): the executable intent then comes from the repository.
const platformHasExecBits = false

const writeAccess = windows.FILE_WRITE_DATA | windows.FILE_APPEND_DATA | windows.FILE_WRITE_EA | windows.DELETE | windows.WRITE_DAC | windows.WRITE_OWNER |
	windows.GENERIC_WRITE | windows.GENERIC_ALL

// unsafeSnapshotReason is the skip reason for a file the worker must not sync, or "" when it is safe to read. Same rule as Win32-OpenSSH for
// authorized_keys: the owner is the user, Administrators or SYSTEM (an elevated editor creates Administrators-owned files), and nobody else
// can write the file. Another ordinary user's file is reported as owned_by_other_user, never dropped silently.
func unsafeSnapshotReason(path string, _ fs.FileInfo) string {
	attributes, err := windows.GetFileAttributes(windows.StringToUTF16Ptr(path))
	if err != nil || attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return "unsafe_permissions"
	}
	descriptor, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil || descriptor == nil || !descriptor.IsValid() {
		return "unsafe_permissions"
	}
	owner, _, err := descriptor.Owner()
	if err != nil || owner == nil {
		return "unsafe_permissions"
	}
	user, err := windows.GetCurrentThreadEffectiveToken().GetTokenUser()
	if err != nil || user == nil || user.User.Sid == nil {
		return "unsafe_permissions"
	}
	if !trustedSnapshotSID(owner, user.User.Sid) {
		return "owned_by_other_user"
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		return "unsafe_permissions"
	}
	if dacl == nil { // a NULL DACL grants everyone full control
		return "writable_by_other_users"
	}
	for index := uint32(0); index < uint32(dacl.AceCount); index++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if windows.GetAce(dacl, index, &ace) != nil || ace == nil {
			return "unsafe_permissions"
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || ace.Mask&writeAccess == 0 {
			continue
		}
		if sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart)); !trustedSnapshotSID(sid, user.User.Sid) && !sid.IsWellKnown(windows.WinCreatorOwnerSid) {
			return "writable_by_other_users"
		}
	}
	return ""
}

func trustedSnapshotSID(sid, user *windows.SID) bool {
	return sid.Equals(user) || sid.IsWellKnown(windows.WinBuiltinAdministratorsSid) || sid.IsWellKnown(windows.WinLocalSystemSid)
}

// Windows has no POSIX executable bits. Managed regular files use a stable
// private-file policy in snapshots so Windows and Unix peers do not oscillate
// solely because os.FileMode reports synthetic 0666 bits on Windows.
func snapshotFileMode(fs.FileInfo) os.FileMode { return 0o600 }
