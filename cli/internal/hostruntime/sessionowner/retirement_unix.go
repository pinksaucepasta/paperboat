//go:build linux || darwin

package sessionowner

import (
	"errors"
	"golang.org/x/sys/unix"
	"os"
	"path/filepath"
	"syscall"
)

// Native removal has already reaped the process. Never recursively remove this
// directory: unexpected content must remain intact for diagnosis and recovery.
func removeRetiredEndpoint(d Descriptor) error {
	directory := filepath.Dir(d.Endpoint)
	if directory == d.StoreRoot() || directory == d.PrivateRoot() {
		return ErrInvalid
	}
	parent, err := unix.Open(filepath.Dir(directory), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil
		}
		return err
	}
	defer unix.Close(parent)
	var info unix.Stat_t
	if err := unix.Fstat(parent, &info); err != nil {
		return err
	}
	if info.Uid != uint32(os.Geteuid()) || info.Mode&0022 != 0 {
		return ErrInvalid
	}
	name := filepath.Base(directory)
	fd, err := unix.Openat(parent, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil
		}
		return err
	}
	defer unix.Close(fd)
	if err := unix.Fstat(fd, &info); err != nil {
		return err
	}
	if info.Uid != uint32(d.UID) || info.Mode&0777 != 0700 {
		return ErrInvalid
	}
	socket := filepath.Base(d.Endpoint)
	if err := unix.Fstatat(fd, socket, &info, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if !errors.Is(err, unix.ENOENT) {
			return err
		}
	} else {
		if info.Mode&unix.S_IFMT != unix.S_IFSOCK || info.Nlink != 1 || info.Uid != uint32(d.UID) {
			return ErrInvalid
		}
		if err := unix.Unlinkat(fd, socket, 0); err != nil {
			return err
		}
	}
	return unix.Unlinkat(parent, name, unix.AT_REMOVEDIR)
}

func validateRetiredDeclaration(path string, info os.FileInfo, d Descriptor) error {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st.Nlink != 1 {
		return ErrInvalid
	}
	return validateDescriptorCustody(path, info, d)
}
