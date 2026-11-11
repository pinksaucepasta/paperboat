//go:build !windows

package service

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"

	"github.com/pinksaucepasta/paperboat/internal/ownerpath"
)

func prepareAtomicDirectory(directory string) error {
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return err
	}
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return ErrInvalidDefinition
	}
	if err := os.Chmod(directory, 0o755); err != nil {
		return err
	}
	info, err = os.Lstat(directory)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o755 {
		return ErrInvalidDefinition
	}
	// As Windows checkWindowsParentCustody does: the directory is this account's or root's, and so is every
	// ancestor, so no other account can swap a parent and redirect a unit or plist written here.
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || (int(stat.Uid) != os.Geteuid() && stat.Uid != 0) {
		return ErrInvalidDefinition
	}
	if err := ownerpath.CheckAncestors(filepath.Clean(directory), os.Geteuid()); err != nil {
		return errors.Join(ErrInvalidDefinition, err)
	}
	return nil
}

func syncServiceDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
