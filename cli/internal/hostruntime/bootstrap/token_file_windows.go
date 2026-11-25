//go:build windows

package bootstrap

import (
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

// insecureEnrollmentTokenFile returns why the file is refused, or "".
func insecureEnrollmentTokenFile(path string, info os.FileInfo) string {
	if info == nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() == 0 || info.Size() > 512 || !windowsHasSingleLink(path) {
		return tokenFileShape
	}
	attributes, err := windows.GetFileAttributes(windows.StringToUTF16Ptr(path))
	if err != nil || attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return tokenFileShape
	}
	user, err := windows.GetCurrentThreadEffectiveToken().GetTokenUser()
	if err != nil || user == nil || user.User.Sid == nil || !user.User.Sid.IsValid() {
		return "the account running pb could not be read"
	}
	descriptor, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil || descriptor == nil || !descriptor.IsValid() {
		return "its permissions could not be read"
	}
	owner, _, err := descriptor.Owner()
	if err != nil || owner == nil || !owner.Equals(user.User.Sid) {
		return "the account running pb must own it (icacls FILE /setowner USER)"
	}
	if !windowsOwnerOnlyDACL(descriptor, user.User.Sid) {
		return "only its owner and SYSTEM may have access, both full control and not inherited " +
			"(icacls FILE /inheritance:r /grant:r USER:F SYSTEM:F, then icacls FILE /remove for every other entry)"
	}
	return ""
}

func windowsOwnerOnlyDACL(descriptor *windows.SECURITY_DESCRIPTOR, user *windows.SID) bool {
	control, _, err := descriptor.Control()
	if err != nil || control&windows.SE_DACL_PROTECTED == 0 {
		return false
	}
	acl, _, err := descriptor.DACL()
	if err != nil || acl == nil {
		return false
	}
	// Both allow ACEs grant the same rights and commute. Windows may reorder
	// them when serializing a descriptor; authorization depends on the exact
	// principal set, masks and inheritance, never that presentation order.
	wanted := map[string]bool{"S-1-5-18": false, user.String(): false}
	if int(acl.AceCount) != len(wanted) {
		return false
	}
	for index := uint32(0); index < uint32(acl.AceCount); index++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		// FILE_ALL_ACCESS from winnt.h: standard rights, synchronize, and
		// all nine file-specific access bits.
		const fileAllAccess = windows.STANDARD_RIGHTS_REQUIRED | windows.SYNCHRONIZE | 0x1ff
		if windows.GetAce(acl, index, &ace) != nil || ace == nil || ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || ace.Header.AceFlags != 0 || ace.Mask != fileAllAccess {
			return false
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if !sid.IsValid() {
			return false
		}
		key := sid.String()
		seen, allowed := wanted[key]
		if !allowed || seen {
			return false
		}
		wanted[key] = true
	}
	return true
}
