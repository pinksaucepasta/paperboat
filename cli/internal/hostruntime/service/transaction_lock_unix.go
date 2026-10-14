//go:build !windows

package service

import (
	"errors"
	"github.com/pinksaucepasta/paperboat/internal/atomicfile"
	"golang.org/x/sys/unix"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// lifecycleDirectory walks trusted ancestors by descriptor. Privileged callers
// never consume a journal beneath an account-owned directory. Root-owned sticky
// temporary directories remain usable for isolated tests; their protected child
// must still belong to the executing account.
func lifecycleDirectory(path string, create bool) (int, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return -1, ErrLifecycleInvalid
	}
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, err
	}
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	owner := uint32(os.Geteuid())
	privateAncestor := false
	for index, name := range parts {
		if name == "" {
			continue
		}
		next, err := unix.Openat(fd, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if errors.Is(err, unix.ENOENT) && create {
			mode := uint32(0755)
			if index == len(parts)-1 {
				mode = 0700
			}
			if err = unix.Mkdirat(fd, name, mode); err == nil || errors.Is(err, unix.EEXIST) {
				next, err = unix.Openat(fd, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
			}
		}
		unix.Close(fd)
		if err != nil {
			return -1, err
		}
		fd = next
		var stat unix.Stat_t
		if err := unix.Fstat(fd, &stat); err != nil {
			unix.Close(fd)
			return -1, err
		}
		last := index == len(parts)-1
		sticky := stat.Uid == 0 && stat.Mode&unix.S_ISVTX != 0
		trusted := stat.Uid == 0 || stat.Uid == owner
		if !trusted || stat.Mode&0022 != 0 && !sticky && !privateAncestor || last && (stat.Uid != owner || stat.Mode&0777 != 0700) {
			unix.Close(fd)
			return -1, ErrLifecycleInvalid
		}
		if stat.Uid == owner && stat.Mode&0077 == 0 {
			privateAncestor = true
		}
	}
	return fd, nil
}
func ensureLifecycleDirectory(path string) error {
	fd, err := lifecycleDirectory(path, true)
	if err != nil {
		return err
	}
	return unix.Close(fd)
}
func openLifecycleFile(path string, create bool) (*os.File, error) {
	parent, err := lifecycleDirectory(filepath.Dir(path), create)
	if err != nil {
		return nil, err
	}
	defer unix.Close(parent)
	flags := unix.O_RDONLY | unix.O_NOFOLLOW | unix.O_CLOEXEC | unix.O_NONBLOCK
	if create {
		flags = unix.O_RDWR | unix.O_CREAT | unix.O_NOFOLLOW | unix.O_CLOEXEC | unix.O_NONBLOCK
	}
	fd, err := unix.Openat(parent, filepath.Base(path), flags, 0600)
	if err != nil {
		return nil, err
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		unix.Close(fd)
		return nil, err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Uid != uint32(os.Geteuid()) || stat.Mode&0777 != 0600 || stat.Nlink != 1 {
		unix.Close(fd)
		return nil, ErrLifecycleInvalid
	}
	return os.NewFile(uintptr(fd), path), nil
}
func lockLifecycle(path string) (func() error, error) {
	file, err := openLifecycleFile(path, true)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, ErrLifecycleBusy
		}
		return nil, err
	}
	return func() error { return errors.Join(syscall.Flock(int(file.Fd()), syscall.LOCK_UN), file.Close()) }, nil
}

func writeLifecycleJournal(path string, data []byte) error {
	return atomicfile.Write(path, data, atomicfile.Options{Mode: 0600, OwnerUID: -1, OwnerGID: -1})
}
