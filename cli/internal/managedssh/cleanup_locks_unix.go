//go:build darwin || linux

package managedssh

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// CleanupUnusedLocks is full-uninstall cleanup after both local and native
// writers have stopped. Normal reconciliation deliberately keeps these locks.
func CleanupUnusedLocks(ctx context.Context, home string, ownerUID uint32) (resultErr error) {
	if ctx == nil || !filepath.IsAbs(home) || filepath.Clean(home) != home {
		return ErrAuthorizedKeysConflict
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateOwnedDirectory(home, ownerUID, false); err != nil {
		return err
	}
	directory := filepath.Join(home, ".ssh")
	if err := validateOwnedDirectory(directory, ownerUID, true); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	directoryFD, err := unix.Open(directory, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, unix.Close(directoryFD)) }()
	type ownedLock struct {
		name string
		fd   int
		stat unix.Stat_t
	}
	var locks []ownedLock
	defer func() {
		for _, lock := range locks {
			resultErr = errors.Join(resultErr, unix.Flock(lock.fd, unix.LOCK_UN), unix.Close(lock.fd))
		}
	}()
	for _, name := range []string{".paperboat-config.lock", ".paperboat-authorized-keys.lock"} {
		fd, err := unix.Openat(directoryFD, name, unix.O_RDWR|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
		if errors.Is(err, unix.ENOENT) {
			continue
		}
		if err != nil {
			return err
		}
		var stat unix.Stat_t
		if err := unix.Fstat(fd, &stat); err != nil {
			return errors.Join(err, unix.Close(fd))
		}
		if err := secureOwnedFileDescriptor(fd, ownerUID); err != nil || stat.Nlink != 1 || stat.Size != 0 {
			return errors.Join(ErrAuthorizedKeysConflict, err, unix.Close(fd))
		}
		if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
			return errors.Join(err, unix.Close(fd))
		}
		locks = append(locks, ownedLock{name, fd, stat})
	}
	// Acquire and validate both before removing either: a concurrent writer or
	// malformed sibling must preserve all coordination state.
	for _, lock := range locks {
		var named unix.Stat_t
		if err := unix.Fstatat(directoryFD, lock.name, &named, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return err
		}
		if named.Dev != lock.stat.Dev || named.Ino != lock.stat.Ino || named.Nlink != 1 || named.Size != 0 || named.Uid != ownerUID || named.Mode&unix.S_IFMT != unix.S_IFREG || named.Mode&0777 != 0600 {
			return ErrAuthorizedKeysConflict
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	for _, lock := range locks {
		if err := unix.Unlinkat(directoryFD, lock.name, 0); err != nil {
			return err
		}
	}
	return nil
}
