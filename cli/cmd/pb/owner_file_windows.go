//go:build windows

package main

import (
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

func ownerOnlyRegularFile(path string, info os.FileInfo) bool {
	return validateOwnerOnlyRegularFile(path, info) == nil
}

func validateOwnerOnlyRegularFile(path string, info os.FileInfo) error {
	if info == nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return errOwnerOnlyFileInvalid
	}
	encoded, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	attributes, err := windows.GetFileAttributes(encoded)
	if err != nil {
		return err
	}
	if attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return errOwnerOnlyFileInvalid
	}
	descriptor, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	return checkOwnerOnlyDescriptor(descriptor)
}

// checkOwnerOnlyDescriptor is the Windows form of Unix's "no group or other bits": the DACL is protected, grants
// no one but this user, SYSTEM and Administrators, and the owner (who may rewrite the DACL) is one of them.
func checkOwnerOnlyDescriptor(descriptor *windows.SECURITY_DESCRIPTOR) error {
	if descriptor == nil || !descriptor.IsValid() {
		return errOwnerOnlyFileInvalid
	}
	control, _, err := descriptor.Control()
	if err != nil {
		return err
	}
	if control&windows.SE_DACL_PROTECTED == 0 {
		return errOwnerOnlyFileInvalid
	}
	user, err := windows.GetCurrentThreadEffectiveToken().GetTokenUser()
	if err != nil {
		return err
	}
	trusted := func(sid *windows.SID) bool {
		return sid != nil && (sid.Equals(user.User.Sid) || sid.IsWellKnown(windows.WinLocalSystemSid) || sid.IsWellKnown(windows.WinBuiltinAdministratorsSid))
	}
	owner, _, err := descriptor.Owner()
	if err != nil || !trusted(owner) {
		return errOwnerOnlyFileInvalid
	}
	dacl, _, err := descriptor.DACL()
	if err != nil || dacl == nil {
		return errOwnerOnlyFileInvalid
	}
	for index := range uint32(dacl.AceCount) {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, index, &ace); err != nil {
			return err
		}
		if ace.Header.AceType == windows.ACCESS_DENIED_ACE_TYPE {
			continue
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || !trusted((*windows.SID)(unsafe.Pointer(&ace.SidStart))) {
			return errOwnerOnlyFileInvalid
		}
	}
	return nil
}
