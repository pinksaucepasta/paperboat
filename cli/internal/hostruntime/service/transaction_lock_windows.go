//go:build windows

package service

import (
	"errors"
	"github.com/pinksaucepasta/paperboat/internal/atomicfile"
	"github.com/pinksaucepasta/paperboat/internal/windowssecurity"
	"golang.org/x/sys/windows"
	"os"
	"path/filepath"
	"unsafe"
)

func lifecycleSecurity() (*windows.SID, string, error) {
	token, err := windows.OpenCurrentProcessToken()
	if err != nil {
		return nil, "", err
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err != nil {
		return nil, "", err
	}
	owner := user.User.Sid
	if token.IsElevated() || owner.String() == "S-1-5-18" {
		owner, err = windows.CreateWellKnownSid(windows.WinLocalSystemSid)
		if err != nil {
			return nil, "", err
		}
	}
	descriptor := "O:" + owner.String() + "D:P(A;;FA;;;SY)(A;;FA;;;BA)"
	if owner.String() != "S-1-5-18" {
		descriptor += "(A;;FA;;;" + owner.String() + ")"
	} else {
		descriptor = "O:SYD:P(A;;FA;;;SY)(A;;FA;;;BA)"
	}
	return owner, descriptor, nil
}
func lifecycleOpen(path string, write bool) (windows.Handle, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	access := uint32(windows.READ_CONTROL | windows.GENERIC_READ)
	if write {
		access |= windows.GENERIC_WRITE
	}
	h, err := windows.CreateFile(p, access, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return 0, err
	}
	var info windows.ByHandleFileInformation
	if err = windows.GetFileInformationByHandle(h, &info); err != nil || info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		windows.CloseHandle(h)
		if err == nil {
			err = ErrLifecycleInvalid
		}
		return 0, err
	}
	return h, nil
}
func lifecycleParentSafe(path string, owner *windows.SID) error {
	h, err := lifecycleOpen(path, false)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	sd, err := windows.GetSecurityInfo(h, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	actual, _, err := sd.Owner()
	if err != nil {
		return err
	}
	sy, _ := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	ba, _ := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if !actual.Equals(sy) && !actual.Equals(ba) && !actual.Equals(owner) {
		return ErrLifecycleInvalid
	}
	acl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	if acl == nil {
		return ErrLifecycleInvalid
	}
	for i := uint32(0); i < uint32(acl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err = windows.GetAce(acl, i, &ace); err != nil {
			return err
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			return ErrLifecycleInvalid
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if !sid.Equals(sy) && !sid.Equals(ba) && !sid.Equals(owner) && ace.Mask&0x500D0156 != 0 {
			return ErrLifecycleInvalid
		}
	}
	return nil
}
func pinLifecycleAncestors(path string) (func() error, error) {
	var handles []windows.Handle
	closeAll := func() error {
		var joined error
		for i := len(handles) - 1; i >= 0; i-- {
			joined = errors.Join(joined, windows.CloseHandle(handles[i]))
		}
		return joined
	}
	var paths []string
	for current := filepath.Clean(path); ; current = filepath.Dir(current) {
		paths = append(paths, current)
		if filepath.Dir(current) == current {
			break
		}
	}
	for i := len(paths) - 1; i >= 0; i-- {
		h, err := lifecycleOpen(paths[i], false)
		if err != nil {
			closeAll()
			return nil, err
		}
		handles = append(handles, h)
	}
	return closeAll, nil
}
func ensureLifecycleDirectory(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return ErrLifecycleInvalid
	}
	owner, descriptor, err := lifecycleSecurity()
	if err != nil {
		return err
	}
	// The managed native parent is already protected. Reject reparse ancestors
	// before creating a private child; never repair an existing untrusted journal root.
	release, err := pinLifecycleAncestors(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer release()
	if err = lifecycleParentSafe(filepath.Dir(path), owner); err != nil {
		return err
	}
	if _, err = os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		if err = os.Mkdir(path, 0700); err != nil {
			return err
		}
		sd, e := windows.SecurityDescriptorFromString(descriptor)
		if e != nil {
			return e
		}
		acl, _, e := sd.DACL()
		if e != nil {
			return e
		}
		apply := func() error {
			return windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, owner, nil, acl, nil)
		}
		if owner.String() == "S-1-5-18" {
			err = windowssecurity.WithRestorePrivilege(apply)
		} else {
			err = apply()
		}
		if err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	h, err := lifecycleOpen(path, false)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	if !windowssecurity.HandleOwnerMatchesSID(h, owner) || !windowssecurity.ProtectedHandleDACLMatches(h, descriptor) {
		return protectUnusedLifecycleDirectory(path, owner, descriptor)
	}
	return nil
}
func openLifecycleFile(path string, create bool) (*os.File, error) {
	if err := ensureLifecycleDirectory(filepath.Dir(path)); err != nil {
		return nil, err
	}
	owner, descriptor, err := lifecycleSecurity()
	if err != nil {
		return nil, err
	}
	if create {
		sd, e := windows.SecurityDescriptorFromString(descriptor)
		if e != nil {
			return nil, e
		}
		creation := sd
		administrators, _ := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
		if owner.String() == "S-1-5-18" {
			creation, e = windows.SecurityDescriptorFromString("D:P(A;;FA;;;SY)(A;;FA;;;BA)")
			if e != nil {
				return nil, e
			}
		}
		attributes := windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: creation}
		p, e := windows.UTF16PtrFromString(path)
		if e != nil {
			return nil, e
		}
		var handle windows.Handle
		createFile := func() error {
			var e error
			handle, e = windows.CreateFile(p, windows.GENERIC_READ|windows.GENERIC_WRITE|windows.READ_CONTROL|windows.WRITE_OWNER, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, &attributes, windows.CREATE_NEW, windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
			return e
		}
		if owner.String() == "S-1-5-18" {
			e = windowssecurity.WithRestorePrivilegeAndOwner(administrators, createFile)
		} else {
			e = createFile()
		}
		if e == nil {
			if owner.String() == "S-1-5-18" {
				e = windowssecurity.WithRestorePrivilege(func() error {
					return windows.SetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION, owner, nil, nil, nil)
				})
			}
			closeErr := windows.CloseHandle(handle)
			if e != nil {
				return nil, errors.Join(e, closeErr, os.Remove(path))
			}
			if closeErr != nil {
				return nil, closeErr
			}
		} else if !errors.Is(e, windows.ERROR_FILE_EXISTS) && !errors.Is(e, windows.ERROR_ALREADY_EXISTS) {
			return nil, e
		}
	}

	h, err := lifecycleOpen(path, create)
	if err != nil {
		return nil, err
	}
	var info windows.ByHandleFileInformation
	err = windows.GetFileInformationByHandle(h, &info)
	if err != nil || info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0 || info.NumberOfLinks != 1 || !windowssecurity.HandleOwnerMatchesSID(h, owner) || !windowssecurity.ProtectedHandleDACLMatches(h, descriptor) {
		windows.CloseHandle(h)
		if err == nil {
			err = ErrLifecycleInvalid
		}
		return nil, err
	}
	return os.NewFile(uintptr(h), path), nil
}
func writeLifecycleJournal(path string, data []byte) error {
	_, descriptor, err := lifecycleSecurity()
	if err != nil {
		return err
	}
	return atomicfile.Write(path, data, atomicfile.Options{Mode: 0600, OwnerUID: -1, OwnerGID: -1, SecurityDescriptor: descriptor})
}
func lockLifecycle(path string) (func() error, error) {
	release, err := pinLifecycleAncestors(filepath.Dir(filepath.Dir(path)))
	if err != nil {
		return nil, err
	}
	file, err := openLifecycleFile(path, true)
	if err != nil {
		release()
		return nil, err
	}
	root, err := lifecycleOpen(filepath.Dir(path), false)
	if err != nil {
		file.Close()
		release()
		return nil, err
	}
	var overlap windows.Overlapped
	if err = windows.LockFileEx(windows.Handle(file.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &overlap); err != nil {
		file.Close()
		windows.CloseHandle(root)
		release()
		if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
			return nil, ErrLifecycleBusy
		}
		return nil, err
	}
	return func() error {
		return errors.Join(windows.UnlockFileEx(windows.Handle(file.Fd()), 0, 1, 0, &overlap), file.Close(), windows.CloseHandle(root), release())
	}, nil
}
