//go:build linux || darwin

package runtime

import (
	"errors"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

func lockCarrierFile(path string) (func() error, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	fd, err := unix.Open(path, unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil || stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Mode&0777 != 0600 || int(stat.Uid) != os.Geteuid() || stat.Nlink != 1 {
		return nil, errors.Join(ErrProductionInvalid, err, f.Close())
	}
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return nil, errors.Join(err, f.Close())
	}
	return f.Close, nil
}

// carrierLockBusy reports a role lock another carrier holds.
func carrierLockBusy(err error) bool { return errors.Is(err, unix.EWOULDBLOCK) }
