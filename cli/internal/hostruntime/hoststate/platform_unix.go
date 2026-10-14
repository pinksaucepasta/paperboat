//go:build darwin || linux

package hoststate

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"syscall"

	"golang.org/x/sys/unix"
)

type processLock struct {
	file *os.File
}

func ensurePrivateDirectory(name string) error {
	_, statErr := os.Lstat(name)
	created := errors.Is(statErr, os.ErrNotExist)
	if statErr != nil && !created {
		return statErr
	}
	if err := os.MkdirAll(name, 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(name)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || fileOwner(info) != os.Geteuid() {
		if err != nil {
			return err
		}
		return ErrInvalidState
	}
	if err := os.Chmod(name, 0o700); err != nil {
		return err
	}
	if created {
		return syncDirectory(filepath.Dir(name))
	}
	return nil
}

func acquireProcessLock(name string) (*processLock, error) {
	_, statErr := os.Lstat(name)
	created := errors.Is(statErr, os.ErrNotExist)
	if statErr != nil && !created {
		return nil, statErr
	}
	fd, err := unix.Open(name, unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		if errors.Is(err, unix.ELOOP) {
			return nil, ErrInvalidState
		}
		return nil, err
	}
	file := os.NewFile(uintptr(fd), name)
	if file == nil {
		return nil, safeStoreFailure("host state lock could not be opened", ErrInvalidState, unix.Close(fd))
	}
	locked := false
	closeWith := func(cause error) (*processLock, error) {
		causes := []error{cause}
		if locked {
			if unlockErr := syscall.Flock(int(file.Fd()), syscall.LOCK_UN); unlockErr != nil {
				causes = append(causes, unlockErr)
			}
			locked = false
		}
		if closeErr := file.Close(); closeErr != nil {
			causes = append(causes, closeErr)
		}
		return nil, safeStoreFailure("host state lock could not be initialized", causes...)
	}
	if err := file.Chmod(0o600); err != nil {
		return closeWith(err)
	}
	info, err := file.Stat()
	pathInfo, pathErr := os.Lstat(name)
	if err != nil {
		return closeWith(safeStoreFailure("host state lock could not be checked", ErrInvalidState, err))
	}
	if pathErr != nil {
		return closeWith(safeStoreFailure("host state lock path could not be checked", ErrInvalidState, pathErr))
	}
	if !info.Mode().IsRegular() || pathInfo.Mode()&os.ModeSymlink != 0 || !os.SameFile(info, pathInfo) || fileOwner(info) != os.Geteuid() || fileLinkCount(info) != 1 || info.Mode().Perm()&0o077 != 0 {
		return closeWith(ErrInvalidState)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return closeWith(ErrLocked)
		}
		return closeWith(err)
	}
	locked = true
	if err = file.Truncate(0); err == nil {
		_, err = file.Seek(0, 0)
	}
	if err == nil {
		_, err = file.WriteString(strconv.Itoa(os.Getpid()) + "\n")
	}
	if err == nil {
		err = file.Sync()
	}
	if err != nil {
		return closeWith(fmt.Errorf("write host state lock owner: %w", err))
	}
	if created {
		if err := syncDirectory(filepath.Dir(name)); err != nil {
			return closeWith(fmt.Errorf("sync host state lock parent: %w", err))
		}
	}
	return &processLock{file: file}, nil
}

func (l *processLock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	file := l.file
	l.file = nil
	unlockErr := syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	closeErr := file.Close()
	if unlockErr == nil && closeErr == nil {
		return nil
	}
	return safeStoreFailure("host state lock could not be released", unlockErr, closeErr)
}

func readPrivateFile(name string, limit int64) (body []byte, resultErr error) {
	info, err := os.Lstat(name)
	if err != nil {
		return nil, safeStoreFailure("host state file could not be inspected", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 || fileOwner(info) != os.Geteuid() || info.Size() < 0 || info.Size() > limit {
		return nil, ErrInvalidState
	}
	fd, err := unix.Open(name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		if errors.Is(err, unix.ELOOP) {
			return nil, safeStoreFailure("host state file could not be opened", ErrInvalidState, err)
		}
		return nil, safeStoreFailure("host state file could not be opened", err)
	}
	file := os.NewFile(uintptr(fd), name)
	if file == nil {
		return nil, safeStoreFailure("host state file could not be opened", ErrInvalidState, unix.Close(fd))
	}
	defer func() {
		if closeErr := file.Close(); closeErr != nil {
			body = nil
			if resultErr == nil {
				resultErr = safeStoreFailure("host state file could not be closed", closeErr)
			} else {
				resultErr = safeStoreFailure("host state file read and close failed", resultErr, closeErr)
			}
		}
	}()
	opened, err := file.Stat()
	if err != nil {
		return nil, safeStoreFailure("host state file could not be checked", ErrInvalidState, err)
	}
	if !os.SameFile(info, opened) || fileLinkCount(opened) != 1 {
		return nil, ErrInvalidState
	}
	buffer, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, safeStoreFailure("host state file could not be read", err)
	}
	if int64(len(buffer)) != opened.Size() || int64(len(buffer)) > limit {
		return nil, ErrInvalidState
	}
	return buffer, nil
}

func syncDirectory(name string) error {
	directory, err := os.Open(filepath.Clean(name))
	if err != nil {
		return err
	}
	return errors.Join(directory.Sync(), directory.Close())
}

func fileOwner(info os.FileInfo) int {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return -1
	}
	return int(stat.Uid)
}

func fileLinkCount(info os.FileInfo) uint64 {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0
	}
	return uint64(stat.Nlink)
}
