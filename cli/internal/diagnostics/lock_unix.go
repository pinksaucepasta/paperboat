//go:build darwin || linux

package diagnostics

import (
	"context"
	"errors"
	"os"
	"syscall"
	"time"
)

// The lock inode is permanent. Replacing or removing it would let two writers
// independently rotate or repair the same shared diagnostic directory.
func acquireDiskLock(ctx context.Context, path string, owner diagnosticOwner) (func() error, error) {
	fd, err := syscall.Open(path, syscall.O_CREAT|syscall.O_RDWR|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	closeError := func(err error) (func() error, error) { _ = file.Close(); return nil, err }
	info, err := file.Stat()
	if err != nil || !validDiagnosticFile(path, info, owner) {
		return closeError(errors.Join(ErrInvalid, err))
	}
	pathInfo, err := os.Lstat(path)
	if err != nil || !os.SameFile(info, pathInfo) {
		return closeError(errors.Join(ErrInvalid, err))
	}
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return closeError(err)
		}
		err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() error { return errors.Join(syscall.Flock(fd, syscall.LOCK_UN), file.Close()) }, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			return closeError(err)
		}
		select {
		case <-ctx.Done():
			return closeError(ctx.Err())
		case <-ticker.C:
		}
	}
}
