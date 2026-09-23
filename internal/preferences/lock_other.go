//go:build !darwin && !linux && !windows

package preferences

import (
	"os"
	"path/filepath"
)

func lockPreferences(path string) (string, func() error, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", nil, err
	}
	if err = os.MkdirAll(filepath.Dir(abs), 0700); err != nil {
		return "", nil, err
	}
	return abs, func() error { return nil }, nil
}
