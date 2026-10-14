//go:build windows

package sessionowner

import (
	"errors"
	"github.com/pinksaucepasta/paperboat/internal/windowssecurity"
	"golang.org/x/sys/windows"
	"os"
)

func currentAccount(d Descriptor) bool {
	token, err := windows.OpenCurrentProcessToken()
	if err != nil {
		return false
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	return err == nil && user.User.Sid.String() == d.OwnerSID
}

func openPrivate(path string, create bool) (*os.File, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	access, disposition := uint32(windows.GENERIC_READ), uint32(windows.OPEN_EXISTING)
	if create {
		access |= windows.GENERIC_WRITE
		disposition = windows.OPEN_ALWAYS
	}
	h, err := windows.CreateFile(p, access, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, disposition, windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(h), path), nil
}
func validatePrivate(f *os.File, path string, d Descriptor) error {
	var st windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(f.Fd()), &st); err != nil {
		return errors.Join(ErrInvalid, err)
	}
	if st.FileAttributes&(windows.FILE_ATTRIBUTE_REPARSE_POINT|windows.FILE_ATTRIBUTE_DIRECTORY) != 0 || st.NumberOfLinks != 1 {
		return ErrInvalid
	}
	sid, err := windows.StringToSid(d.OwnerSID)
	if err != nil {
		return errors.Join(ErrInvalid, err)
	}
	owner, err := windowssecurity.CheckOwnerMatchesSID(path, sid)
	if err != nil {
		return errors.Join(ErrInvalid, err)
	}
	acl, err := windowssecurity.CheckProtectedDACLMatches(path, "D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;FA;;;"+d.OwnerSID+")")
	if err != nil {
		return errors.Join(ErrInvalid, err)
	}
	if !owner || !acl {
		return ErrInvalid
	}
	return nil
}
func lockFile(f *os.File) error {
	var region windows.Overlapped
	err := windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &region)
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return errors.Join(ErrBusy, err)
	}
	return err
}
func unlockFile(f *os.File) error {
	var region windows.Overlapped
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, &region)
}
