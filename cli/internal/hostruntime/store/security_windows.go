//go:build windows

package store

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

func secureStoreDirectory(path string) error { return secureStoreObject(path, true) }
func secureStoreFile(path string) error      { return secureStoreObject(path, false) }

func secureStoreObject(path string, directory bool) error {
	info, err := os.Lstat(path)
	if err != nil {
		return storeFailure("runtime store object inspection failed", err)
	}
	if directory != info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return ErrCorrupt
	}
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return storeFailure("runtime store path is invalid", err)
	}
	attributes, err := windows.GetFileAttributes(name)
	if err != nil {
		return storeFailure("runtime store attributes are unavailable", err)
	}
	if attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return ErrCorrupt
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return storeFailure("runtime store owner SID is unavailable", err)
	}
	if user == nil || user.User.Sid == nil || !user.User.Sid.IsValid() {
		return errors.New("Windows store owner SID is unavailable")
	}
	aceFlags := ""
	if directory {
		aceFlags = "OICI"
	}
	descriptor, err := windows.SecurityDescriptorFromString("D:P(A;" + aceFlags + ";FA;;;SY)(A;" + aceFlags + ";FA;;;BA)(A;" + aceFlags + ";FA;;;" + user.User.Sid.String() + ")")
	if err != nil {
		return storeFailure("runtime store custody update failed", err)
	}
	absolute, err := descriptor.ToAbsolute()
	if err != nil {
		return storeFailure("runtime store custody update failed", err)
	}
	dacl, _, err := absolute.DACL()
	if err != nil {
		return storeFailure("runtime store custody update failed", err)
	}
	return storeFailure("runtime store custody update failed", windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil))
}
