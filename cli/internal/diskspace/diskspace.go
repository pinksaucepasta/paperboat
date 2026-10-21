// Package diskspace reports the free space of the disk that holds a path.
package diskspace

import (
	"errors"
	"os"
	"path/filepath"
)

// Available returns the bytes a normal user can still write to the disk holding path. A path that
// does not exist yet is measured through its nearest existing parent, which is the disk it would
// be created on.
func Available(path string) (uint64, error) {
	if !filepath.IsAbs(path) {
		return 0, errors.New("disk space path must be absolute")
	}
	for current := filepath.Clean(path); ; current = filepath.Dir(current) {
		if _, err := os.Stat(current); err == nil {
			return available(current)
		} else if !errors.Is(err, os.ErrNotExist) {
			return 0, err
		}
		if parent := filepath.Dir(current); parent == current {
			return 0, os.ErrNotExist
		}
	}
}
