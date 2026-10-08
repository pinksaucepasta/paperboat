//go:build windows

package main

import (
	"os"

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
	descriptor, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
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
	return nil
}
