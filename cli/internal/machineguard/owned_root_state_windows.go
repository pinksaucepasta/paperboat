//go:build windows

package machineguard

import (
	"errors"
	"os"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

func validateOwnedRootStateFile(path, _ string, info os.FileInfo) error {
	attributes, ok := info.Sys().(*syscall.Win32FileAttributeData)
	if !ok || attributes.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return errors.New("unsafe machine guard certificate state file; preserved")
	}
	descriptor, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	if descriptor == nil {
		return errors.New("unsafe machine guard certificate state descriptor; preserved")
	}
	owner, _, err := descriptor.Owner()
	if err != nil || owner == nil {
		return errors.New("unsafe machine guard certificate state owner; preserved")
	}
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		return err
	}
	admins, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return err
	}
	if !owner.Equals(system) && !owner.Equals(admins) {
		return errors.New("unsafe machine guard certificate state owner; preserved")
	}
	dacl, _, err := descriptor.DACL()
	if err != nil || dacl == nil {
		return errors.New("unsafe machine guard certificate state ACL; preserved")
	}
	for index := uint32(0); index < uint32(dacl.AceCount); index++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if windows.GetAce(dacl, index, &ace) != nil || ace == nil || ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			return errors.New("unsafe machine guard certificate state ACL; preserved")
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if !sid.IsValid() || (!sid.Equals(system) && !sid.Equals(admins)) {
			return errors.New("unsafe machine guard certificate state ACL; preserved")
		}
	}
	return nil
}
