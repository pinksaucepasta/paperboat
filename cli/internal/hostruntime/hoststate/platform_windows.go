//go:build windows

package hoststate

import (
	"errors"
	"io"
	"os"
	"runtime"
	"strconv"

	"github.com/pinksaucepasta/paperboat/internal/windowssecurity"
	"golang.org/x/sys/windows"
)

type processLock struct {
	file   *os.File
	region windows.Overlapped
}

func ensurePrivateDirectory(name string) error {
	if err := os.MkdirAll(name, 0o700); err != nil {
		return safeStoreFailure("host state directory could not be created", err)
	}
	return protectWindowsObject(name, true)
}

func acquireProcessLock(name string) (*processLock, error) {
	file, err := os.OpenFile(name, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, safeStoreFailure("host state lock could not be opened", err)
	}
	closeWith := func(cause error) (*processLock, error) {
		return nil, safeStoreFailure("host state lock acquisition failed", cause, file.Close())
	}
	if err := protectWindowsObject(name, false); err != nil {
		return closeWith(err)
	}
	lock := &processLock{file: file}
	err = windows.LockFileEx(windows.Handle(file.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &lock.region)
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return closeWith(safeStoreFailure("host state is locked by another process", ErrLocked, err))
	}
	if err != nil {
		return closeWith(err)
	}
	err = file.Truncate(0)
	if err == nil {
		_, err = file.Seek(0, 0)
	}
	if err == nil {
		_, err = file.WriteString(strconv.Itoa(os.Getpid()) + "\r\n")
	}
	if err == nil {
		err = file.Sync()
	}
	if err != nil {
		unlockErr := windows.UnlockFileEx(windows.Handle(file.Fd()), 0, 1, 0, &lock.region)
		return closeWith(safeStoreFailure("host state lock owner could not be persisted", err, unlockErr))
	}
	return lock, nil
}

func (l *processLock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	file := l.file
	l.file = nil
	unlockErr := windows.UnlockFileEx(windows.Handle(file.Fd()), 0, 1, 0, &l.region)
	closeErr := file.Close()
	if unlockErr == nil && closeErr == nil {
		return nil
	}
	return safeStoreFailure("host state lock could not be released", unlockErr, closeErr)
}

func readPrivateFile(name string, limit int64) (data []byte, resultErr error) {
	if limit <= 0 || limit > MaxStateBytes {
		return nil, ErrInvalidState
	}
	if err := validateWindowsFile(name); err != nil {
		return nil, err
	}
	if err := protectWindowsObject(name, false); err != nil {
		return nil, err
	}
	before, err := os.Lstat(name)
	if err != nil {
		return nil, safeStoreFailure("host state file could not be inspected", err)
	}
	if !before.Mode().IsRegular() || before.Mode()&os.ModeSymlink != 0 || before.Size() < 0 || before.Size() > limit {
		return nil, ErrInvalidState
	}
	file, err := os.Open(name)
	if err != nil {
		return nil, safeStoreFailure("host state file could not be opened", err)
	}
	defer func() {
		if closeErr := file.Close(); closeErr != nil {
			resultErr = safeStoreFailure("host state file read or close failed", resultErr, closeErr)
			clear(data)
			data = nil
		}
	}()
	opened, err := file.Stat()
	if err != nil {
		return nil, safeStoreFailure("open host state file could not be inspected", err)
	}
	if !os.SameFile(before, opened) || !opened.Mode().IsRegular() || opened.Size() < 0 || opened.Size() > limit {
		return nil, ErrInvalidState
	}
	data, err = io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		clear(data)
		return nil, safeStoreFailure("host state file could not be read", err)
	}
	after, err := file.Stat()
	if err != nil {
		clear(data)
		return nil, safeStoreFailure("host state file could not be inspected after reading", err)
	}
	if int64(len(data)) > limit || int64(len(data)) != opened.Size() || !os.SameFile(opened, after) || opened.Size() != after.Size() || !opened.ModTime().Equal(after.ModTime()) {
		clear(data)
		return nil, ErrInvalidState
	}
	return data, nil
}

// State publication uses MOVEFILE_WRITE_THROUGH. A directory FlushFileBuffers
// call is not consistently supported on Windows filesystems, so no additional
// directory operation is required here.
func syncDirectory(string) error { return nil }

func validateWindowsFile(name string) error {
	info, err := os.Lstat(name)
	if err != nil {
		return safeStoreFailure("host state file could not be inspected", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return ErrInvalidState
	}
	encoded, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return safeStoreFailure("host state path could not be encoded", err)
	}
	attributes, err := windows.GetFileAttributes(encoded)
	if err != nil {
		return safeStoreFailure("host state object attributes could not be inspected", err)
	}
	if attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return ErrInvalidState
	}
	return nil
}

func protectWindowsObject(name string, directory bool) (resultErr error) {
	info, err := os.Lstat(name)
	if err != nil || info.IsDir() != directory || info.Mode()&os.ModeSymlink != 0 {
		if err != nil {
			return safeStoreFailure("host state object could not be inspected", err)
		}
		return ErrInvalidState
	}
	encoded, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return safeStoreFailure("host state path could not be encoded", err)
	}
	attributes, err := windows.GetFileAttributes(encoded)
	if err != nil {
		return safeStoreFailure("host state object attributes could not be inspected", err)
	}
	if attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return ErrInvalidState
	}
	token, err := windows.OpenCurrentProcessToken()
	if err != nil {
		return safeStoreFailure("host state owner token could not be opened", err)
	}
	defer func() {
		if closeErr := token.Close(); closeErr != nil {
			resultErr = safeStoreFailure("host state owner token could not be closed", resultErr, closeErr)
		}
	}()
	user, err := token.GetTokenUser()
	if err != nil {
		return safeStoreFailure("host state owner SID could not be inspected", err)
	}
	if user == nil || user.User.Sid == nil || !user.User.Sid.IsValid() {
		return ErrInvalidState
	}
	flags := ""
	if directory {
		flags = "OICI"
	}
	sddl := "D:P(A;" + flags + ";FA;;;SY)(A;" + flags + ";FA;;;BA)(A;" + flags + ";FA;;;" + user.User.Sid.String() + ")"
	descriptor, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		return safeStoreFailure("host state security descriptor could not be applied", err)
	}
	absolute, err := descriptor.ToAbsolute()
	if err != nil {
		return safeStoreFailure("host state security descriptor could not be applied", err)
	}
	dacl, _, err := absolute.DACL()
	if err != nil {
		return safeStoreFailure("host state security descriptor could not be applied", err)
	}
	if err := windows.SetNamedSecurityInfo(name, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
		return safeStoreFailure("host state security descriptor could not be applied", err)
	}
	runtime.KeepAlive(absolute)
	matches, err := windowssecurity.CheckProtectedDACLMatches(name, sddl)
	if err != nil {
		return safeStoreFailure("host state protected ACL could not be inspected", err)
	}
	if !matches {
		return ErrInvalidState
	}
	return nil
}
