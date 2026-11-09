package atomicfile

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// AbsentOrBlocked returns err unchanged unless it reports path as missing while an ancestor of path is a file.
// Unix reports that as ENOTDIR by itself. Windows reports "path not found" (its syscall.ENOTDIR is that same
// code), so state stored under a file would read as never created; there the result is a *fs.PathError carrying
// ErrNotDirectory, which is not fs.ErrNotExist.
func AbsentOrBlocked(path string, err error) error {
	if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	for dir := filepath.Dir(path); ; dir = filepath.Dir(dir) {
		info, statErr := os.Stat(dir)
		if statErr == nil {
			if !info.IsDir() {
				return &fs.PathError{Op: "stat", Path: path, Err: ErrNotDirectory}
			}
			return err
		}
		if !errors.Is(statErr, fs.ErrNotExist) || filepath.Dir(dir) == dir {
			return err
		}
	}
}
