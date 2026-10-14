//go:build windows

package service

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unsafe"

	"github.com/pinksaucepasta/paperboat/internal/windowssecurity"
	"golang.org/x/sys/windows"
)

// No journal authority is adopted here: the only accepted old contents are an
// empty native lock. Trusted administrators created this unused directory, and
// account readers cannot change it or its protected parent while handles pin it.
func protectUnusedLifecycleDirectory(path string, owner *windows.SID, descriptor string) error {
	if owner.String() != "S-1-5-18" {
		return ErrLifecycleInvalid
	}
	open := func(path string) (windows.Handle, error) {
		p, err := windows.UTF16PtrFromString(path)
		if err != nil {
			return 0, err
		}
		return windows.CreateFile(p, windows.GENERIC_READ|windows.READ_CONTROL|windows.WRITE_DAC|windows.WRITE_OWNER, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	}
	h, err := open(path)
	if err != nil {
		return err
	}
	directory := os.NewFile(uintptr(h), path)
	defer directory.Close()
	if err := unusedLifecycleObject(h, true); err != nil {
		return err
	}
	entries, err := directory.ReadDir(2)
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if len(entries) > 1 || len(entries) == 1 && !strings.EqualFold(entries[0].Name(), "service-lifecycle.lock") {
		return ErrLifecycleInvalid
	}
	var lock windows.Handle
	if len(entries) == 1 {
		lock, err = open(filepath.Join(path, "service-lifecycle.lock"))
		if err != nil {
			return err
		}
		defer windows.CloseHandle(lock)
		if err := unusedLifecycleObject(lock, false); err != nil {
			return err
		}
	}
	sd, err := windows.SecurityDescriptorFromString(descriptor)
	if err != nil {
		return err
	}
	acl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	protect := func(handle windows.Handle) error {
		if err := windowssecurity.WithRestorePrivilege(func() error {
			return windows.SetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, owner, nil, acl, nil)
		}); err != nil {
			return err
		}
		if !windowssecurity.HandleOwnerMatchesSID(handle, owner) || !windowssecurity.ProtectedHandleDACLMatches(handle, descriptor) {
			return ErrLifecycleInvalid
		}
		return nil
	}
	if lock != 0 {
		if err := protect(lock); err != nil {
			return err
		}
	}
	return protect(h)
}

func unusedLifecycleObject(handle windows.Handle, directory bool) error {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		return err
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 || (info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0) != directory || !directory && (info.NumberOfLinks != 1 || info.FileSizeHigh != 0 || info.FileSizeLow != 0) {
		return ErrLifecycleInvalid
	}
	sd, err := windows.GetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	owner, _, err := sd.Owner()
	if err != nil {
		return err
	}
	sy, _ := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	ba, _ := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if !owner.Equals(sy) && !owner.Equals(ba) {
		return ErrLifecycleInvalid
	}
	acl, _, err := sd.DACL()
	if err != nil || acl == nil {
		return errors.Join(ErrLifecycleInvalid, err)
	}
	for index := uint32(0); index < uint32(acl.AceCount); index++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(acl, index, &ace); err != nil {
			return err
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			return ErrLifecycleInvalid
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if !sid.Equals(sy) && !sid.Equals(ba) && ace.Mask&0x500D0156 != 0 {
			return ErrLifecycleInvalid
		}
	}
	return nil
}
