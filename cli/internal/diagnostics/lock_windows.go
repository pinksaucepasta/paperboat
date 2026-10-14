//go:build windows

package diagnostics

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

func acquireDiskLock(ctx context.Context, path string, owner diagnosticOwner) (func() error, error) {
	if err := rejectReparseAncestors(filepath.Dir(path)); err != nil {
		return nil, err
	}
	descriptor, err := windows.SecurityDescriptorFromString(diagnosticSDDL(owner))
	if err != nil {
		return nil, err
	}
	attributes := windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: descriptor}
	pointer, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateFile(pointer, windows.GENERIC_READ|windows.GENERIC_WRITE, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, &attributes, windows.OPEN_ALWAYS, windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	runtime.KeepAlive(descriptor)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(handle), path)
	closeError := func(err error) (func() error, error) { _ = file.Close(); return nil, err }
	info, err := file.Stat()
	if err != nil || !validDiagnosticFile(path, info, owner) {
		return closeError(errors.Join(ErrInvalid, err))
	}
	pathInfo, err := os.Lstat(path)
	if err != nil || !os.SameFile(info, pathInfo) {
		return closeError(errors.Join(ErrInvalid, err))
	}
	var region windows.Overlapped
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return closeError(err)
		}
		err := windows.LockFileEx(handle, windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &region)
		if err == nil {
			return func() error { return errors.Join(windows.UnlockFileEx(handle, 0, 1, 0, &region), file.Close()) }, nil
		}
		if !errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
			return closeError(err)
		}
		select {
		case <-ctx.Done():
			return closeError(ctx.Err())
		case <-ticker.C:
		}
	}
}
