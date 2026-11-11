//go:build windows

package filelock

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

func open(path string) (*os.File, error) { return os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600) }

func tryLock(file *os.File) (busy bool, err error) {
	var overlapped windows.Overlapped
	err = windows.LockFileEx(windows.Handle(file.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &overlapped)
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return true, nil
	}
	return false, err
}

func unlock(file *os.File) error {
	var overlapped windows.Overlapped
	return errors.Join(windows.UnlockFileEx(windows.Handle(file.Fd()), 0, 1, 0, &overlapped), file.Close())
}
