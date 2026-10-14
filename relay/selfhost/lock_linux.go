package selfhost

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

func lockState(dir string) (*os.File, error) {
	if !filepath.IsAbs(dir) {
		return nil, errors.New("installation directory must be absolute")
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("installation directory must be a real private directory")
	}
	fd, err := syscall.Open(filepath.Join(dir, ".selfhost.lock"), syscall.O_CREAT|syscall.O_RDWR|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), "installation lock")
	if err = syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("another self-host command is running; wait for it to finish: %w", err)
	}
	return f, nil
}
