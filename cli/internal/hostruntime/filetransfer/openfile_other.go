//go:build !windows

package filetransfer

import (
	"errors"
	"os"
	"syscall"
)

// openSpoolFile opens a spool file without following a symlink and refuses anything but a regular file with one
// link, so a swapped .part or .content cannot redirect a write or a verification outside Root.
func openSpoolFile(path string, write bool) (*os.File, error) {
	flag := os.O_RDONLY
	if write {
		flag = os.O_WRONLY
	}
	file, err := os.OpenFile(path, flag|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err == nil {
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !info.Mode().IsRegular() || !ok || stat.Nlink != 1 {
			err = errors.New("spool file is not a regular file with one link")
		}
	}
	if err != nil {
		return nil, errors.Join(err, file.Close())
	}
	return file, nil
}
