//go:build darwin || linux

package preferences

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

func lockPreferences(path string) (string, func() error, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", nil, err
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0700); err != nil {
		return "", nil, fmt.Errorf("create preferences directory: %w", err)
	}
	file, err := os.OpenFile(abs+".lock", os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return "", nil, fmt.Errorf("open preferences lock: %w", err)
	}
	if err = file.Chmod(0600); err != nil {
		file.Close()
		return "", nil, err
	}
	if err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return "", nil, ErrBusy
		}
		return "", nil, fmt.Errorf("lock preferences: %w", err)
	}
	return abs, func() error { return errors.Join(syscall.Flock(int(file.Fd()), syscall.LOCK_UN), file.Close()) }, nil
}
