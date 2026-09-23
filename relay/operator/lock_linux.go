package operator

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

func lockOperator(dir string) (*os.File, error) {
	if !filepath.IsAbs(dir) {
		return nil, errors.New("operator directory must be absolute")
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("operator directory must be a real private directory")
	}
	fd, err := syscall.Open(filepath.Join(dir, ".operator.lock"), syscall.O_CREAT|syscall.O_RDWR|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), "operator lock")
	if err = syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, errors.New("another operator command is running; wait for it to finish")
	}
	return f, nil
}
