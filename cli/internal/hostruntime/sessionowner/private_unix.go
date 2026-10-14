//go:build linux || darwin

package sessionowner

import (
	"errors"
	"golang.org/x/sys/unix"
	"os"
)

func currentAccount(d Descriptor) bool { return os.Geteuid() == d.UID && os.Getegid() == d.GID }

func openPrivate(path string, create bool) (*os.File, error) {
	flags := unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NOFOLLOW
	if create {
		flags = unix.O_RDWR | unix.O_CREAT | unix.O_CLOEXEC | unix.O_NOFOLLOW
	}
	fd, err := unix.Open(path, flags, 0600)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), path), nil
}
func validatePrivate(f *os.File, path string, d Descriptor) error {
	var st unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &st); err != nil {
		return errors.Join(ErrInvalid, err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return errors.Join(ErrInvalid, err)
	}
	opened, err := f.Stat()
	if err != nil {
		return errors.Join(ErrInvalid, err)
	}
	if !opened.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || !os.SameFile(info, opened) || st.Nlink != 1 || int(st.Uid) != d.UID || opened.Mode().Perm() != 0600 {
		return ErrInvalid
	}
	return nil
}
func lockFile(f *os.File) error {
	err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
		return errors.Join(ErrBusy, err)
	}
	return err
}
func unlockFile(f *os.File) error { return unix.Flock(int(f.Fd()), unix.LOCK_UN) }
