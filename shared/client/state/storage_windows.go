//go:build windows

package state

import (
	"errors"
	"os"
	"strings"

	"golang.org/x/sys/windows"
)

func securePath(path string, dir bool) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || info.IsDir() != dir {
		return errors.New("SDK state path has an invalid type")
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err
	}
	existing, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	owner, _, err := existing.Owner()
	if err != nil {
		return err
	}
	if !owner.Equals(user.User.Sid) {
		return errors.New("SDK state path belongs to another user")
	}
	sd, err := windows.SecurityDescriptorFromString(privateSDDL(user.User.Sid.String()))
	if err != nil {
		return err
	}
	acl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, acl, nil)
}
func openPrivate(path string) (*os.File, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateFile(p, windows.GENERIC_READ, windows.FILE_SHARE_READ, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(handle), path), nil
}
func validatePrivate(file *os.File) error {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(file.Fd()), &info); err != nil {
		return err
	}
	if info.FileAttributes&(windows.FILE_ATTRIBUTE_REPARSE_POINT|windows.FILE_ATTRIBUTE_DIRECTORY) != 0 || info.NumberOfLinks != 1 {
		return errors.New("SDK session state must be a private regular file")
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err
	}
	sd, err := windows.GetSecurityInfo(windows.Handle(file.Fd()), windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	owner, _, err := sd.Owner()
	if err != nil {
		return err
	}
	if !owner.Equals(user.User.Sid) {
		return errors.New("SDK session state belongs to another user")
	}
	control, _, err := sd.Control()
	if err != nil {
		return err
	}
	if control&windows.SE_DACL_PROTECTED == 0 {
		return errors.New("SDK session state has inherited permissions")
	}
	want, err := windows.SecurityDescriptorFromString(privateSDDL(user.User.Sid.String()))
	if err != nil {
		return err
	}
	actual := daclSDDL(sd.String())
	expected := daclSDDL(want.String())
	if actual == "" || actual != expected {
		return errors.New("SDK session state is not owner-only")
	}
	return nil
}
func lockDirectory(path string) (func() error, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = securePath(path, false); err == nil {
		err = validatePrivate(file)
	}
	var region windows.Overlapped
	if err == nil {
		err = windows.LockFileEx(windows.Handle(file.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &region)
	}
	if err != nil {
		_ = file.Close()
		if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
			return nil, ErrDirectoryBusy
		}
		return nil, err
	}
	return func() error {
		return errors.Join(windows.UnlockFileEx(windows.Handle(file.Fd()), 0, 1, 0, &region), file.Close())
	}, nil
}
func syncDirectory(string) error { return nil } // Windows file Sync precedes the atomic replacement.

func privateSDDL(sid string) string { return "D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;FA;;;" + sid + ")" }
func daclSDDL(text string) string {
	_, value, ok := strings.Cut(text, "D:")
	if !ok {
		return ""
	}
	flags, aces, ok := strings.Cut(value, "(")
	if !ok {
		return ""
	}
	return strings.ReplaceAll(flags, "AI", "") + "(" + aces
}
