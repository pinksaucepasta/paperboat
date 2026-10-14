//go:build !windows

package hostinstall

import (
	"errors"
	"golang.org/x/sys/unix"
	"io"
	"os"
)

// Read the authenticated inode, not bytes reopened after a pathname check.
// This also protects existing root-owned enrollment records beneath a shared
// account directory without treating that directory as journal authority.
func readProtectedInstallState(path string, maximum int64) (body []byte, resultErr error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	defer func() {
		resultErr = errors.Join(resultErr, file.Close())
		if resultErr != nil {
			body = nil
		}
	}()
	var stat unix.Stat_t
	if err = unix.Fstat(fd, &stat); err != nil {
		return nil, err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Uid != 0 || stat.Mode&0777 != 0600 || stat.Nlink != 1 || stat.Size < 1 || stat.Size > maximum {
		return nil, ErrInvalidRequest
	}
	body, err = io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > maximum {
		return nil, ErrInvalidRequest
	}
	return body, nil
}
