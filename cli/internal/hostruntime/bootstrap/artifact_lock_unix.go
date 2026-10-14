//go:build !windows

package bootstrap

import (
	"errors"
	"golang.org/x/sys/unix"
	"os"
)

func openArtifactLock(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	if err := validateArtifactLock(f, path); err != nil {
		f.Close()
		return nil, err
	}
	info, err := f.Stat()
	if err != nil || info.Mode().Perm() != 0600 {
		f.Close()
		return nil, ErrArtifactTarget
	}
	var st unix.Stat_t
	if err = unix.Fstat(fd, &st); err != nil || st.Nlink != 1 {
		f.Close()
		return nil, ErrArtifactTarget
	}
	return f, nil
}
func tryArtifactLock(f *os.File) (bool, error) {
	err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
		return false, nil
	}
	return err == nil, err
}
func releaseArtifactLock(f *os.File) { _ = unix.Flock(int(f.Fd()), unix.LOCK_UN) }
