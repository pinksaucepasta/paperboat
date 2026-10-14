//go:build linux || darwin

package sessionowner

import (
	"errors"
	"golang.org/x/sys/unix"
	"os"
	"path/filepath"
	"syscall"

	"github.com/pinksaucepasta/paperboat/internal/atomicfile"
)

func unixDirectory(path string, uid int, mode os.FileMode) error {
	info, err := os.Lstat(path)
	if err != nil {
		return catalogFailure(err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || stat.Uid != uint32(uid) || info.Mode().Perm()&mode != 0 {
		return ErrInvalid
	}
	return nil
}
func checkCatalogRoot(path string, _ string) error {
	for current := path; ; current = filepath.Dir(current) {
		if err := unixDirectory(current, 0, 0022); err != nil {
			return err
		}
		if filepath.Dir(current) == current {
			break
		}
	}
	return nil
}
func prepareCatalogRoot(path string, _ Descriptor) error {
	if err := checkCatalogRoot(filepath.Dir(path), ""); err != nil {
		return err
	}
	if err := os.Mkdir(path, 0755); err != nil && !errors.Is(err, os.ErrExist) {
		return catalogFailure(err)
	}
	return checkCatalogRoot(path, "")
}
func writeCatalogDescriptor(path string, body []byte, _ Descriptor) error {
	return atomicfile.Write(path, body, atomicfile.Options{Mode: 0644, OwnerUID: 0, OwnerGID: 0})
}
func verifyCatalogExecutable(d Descriptor) error {
	info, err := os.Lstat(d.Executable)
	if err != nil {
		return catalogFailure(err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || stat.Uid != 0 || info.Mode().Perm()&0022 != 0 || info.Mode().Perm()&0111 == 0 {
		return ErrInvalid
	}
	if err := checkCatalogRoot(filepath.Dir(d.Executable), ""); err != nil {
		return err
	}
	return d.Source.Verify(d.Executable)
}
func prepareCatalogCapability(d Descriptor) error {
	if err := prepareUnixStoreDirectories(d); err != nil {
		return err
	}
	if info, err := os.Lstat(d.TokenPath()); err == nil {
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || !info.Mode().IsRegular() || stat.Uid != uint32(d.UID) || info.Mode().Perm() != 0600 || info.Size() != 43 {
			return ErrInvalid
		}
		_, err := ReadCapability(d)
		return err
	} else if !errors.Is(err, os.ErrNotExist) {
		return catalogFailure(err)
	}
	body, err := newCapability()
	if err != nil {
		return err
	}
	return atomicfile.Write(d.TokenPath(), body, atomicfile.Options{Mode: 0600, OwnerUID: d.UID, OwnerGID: d.GID})
}

func prepareUnixStoreDirectories(d Descriptor) (resultErr error) {
	root, err := unix.Open(d.RuntimeRoot, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return catalogFailure(err)
	}
	defer func() { resultErr = errors.Join(resultErr, unix.Close(root)) }()
	var stat unix.Stat_t
	if err := unix.Fstat(root, &stat); err != nil {
		return err
	}
	if stat.Uid != uint32(d.UID) || stat.Mode&0077 != 0 {
		return ErrInvalid
	}
	current := root
	for _, name := range []string{"session-owners", d.Generation} {
		if err := unix.Mkdirat(current, name, 0700); err != nil && !errors.Is(err, unix.EEXIST) {
			return err
		}
		next, err := unix.Openat(current, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
		if err != nil {
			return err
		}
		defer func(fd int) { resultErr = errors.Join(resultErr, unix.Close(fd)) }(next)
		if err := unix.Fstat(next, &stat); err != nil {
			return err
		}
		if stat.Uid != uint32(d.UID) && stat.Uid != 0 || stat.Mode&0077 != 0 {
			return ErrInvalid
		}
		if err := unix.Fchown(next, d.UID, d.GID); err != nil {
			return err
		}
		current = next
	}
	return nil
}
