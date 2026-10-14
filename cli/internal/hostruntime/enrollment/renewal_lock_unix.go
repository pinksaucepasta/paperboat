//go:build darwin || linux

package enrollment

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
)

const runtimeIdentityRenewalLockFile = ".runtime-identity-renewal.lock"

type runtimeIdentityRenewalLock struct {
	file *os.File
}

func openRuntimeIdentityRenewalLock(stateRoot string) (*runtimeIdentityRenewalLock, error) {
	if !filepath.IsAbs(stateRoot) || filepath.Clean(stateRoot) != stateRoot {
		return nil, ErrInvalid
	}
	rootInfo, err := os.Lstat(stateRoot)
	if err != nil {
		return nil, err
	}
	rootStat, ok := rootInfo.Sys().(*syscall.Stat_t)
	if !ok || !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 || rootStat.Uid != uint32(os.Geteuid()) || rootInfo.Mode().Perm() != 0o700 {
		return nil, ErrInvalid
	}

	path := filepath.Join(stateRoot, runtimeIdentityRenewalLockFile)
	fd, err := unix.Open(path, unix.O_CREAT|unix.O_EXCL|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	created := err == nil
	if errors.Is(err, unix.EEXIST) {
		fd, err = unix.Open(path, unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	}
	if err != nil {
		if errors.Is(err, unix.ELOOP) {
			return nil, ErrInvalid
		}
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		return nil, errors.Join(ErrInvalid, unix.Close(fd))
	}
	closeWith := func(cause error) (*runtimeIdentityRenewalLock, error) {
		return nil, errors.Join(cause, file.Close())
	}
	if created {
		if err := file.Chmod(0o600); err != nil {
			return closeWith(err)
		}
	}
	opened, err := file.Stat()
	if err != nil {
		return closeWith(err)
	}
	named, err := os.Lstat(path)
	if err != nil {
		return closeWith(err)
	}
	stat, ok := opened.Sys().(*syscall.Stat_t)
	if !ok || !opened.Mode().IsRegular() || named.Mode()&os.ModeSymlink != 0 || !os.SameFile(opened, named) || stat.Uid != uint32(os.Geteuid()) || stat.Nlink != 1 || opened.Mode().Perm() != 0o600 {
		return closeWith(ErrInvalid)
	}
	return &runtimeIdentityRenewalLock{file: file}, nil
}

func (lock *runtimeIdentityRenewalLock) Try() (bool, error) {
	err := unix.Flock(int(lock.file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
		return false, nil
	}
	return err == nil, err
}

func (lock *runtimeIdentityRenewalLock) Close() error {
	if lock == nil || lock.file == nil {
		return nil
	}
	file := lock.file
	lock.file = nil
	return errors.Join(unix.Flock(int(file.Fd()), unix.LOCK_UN), file.Close())
}
