// Package diskspace reports the free space of the disk that holds a path.
package diskspace

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

// Available returns the bytes a normal user can still write to the disk holding path. A path that
// does not exist yet is measured through its nearest existing parent, which is the disk it would
// be created on.
func Available(path string) (uint64, error) {
	existing, err := nearestExisting(path)
	if err != nil {
		return 0, err
	}
	return available(existing)
}

// FatFileLimit is the largest file FAT volumes hold: 4 GiB minus one byte.
const FatFileLimit = 1<<32 - 1

// MaxFile returns the largest single file the disk holding path can store, and the file system's name for
// messages. Zero means no limit that matters here: only FAT volumes (FAT32 on USB sticks and SD cards) have one.
func MaxFile(path string) (limit uint64, name string, err error) {
	existing, err := nearestExisting(path)
	if err != nil {
		return 0, "", err
	}
	name, err = fileSystem(existing)
	if err != nil {
		return 0, "", err
	}
	if name == "FAT" {
		return FatFileLimit, name, nil
	}
	return 0, name, nil
}

func nearestExisting(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", errors.New("disk space path must be absolute")
	}
	for current := filepath.Clean(path); ; current = filepath.Dir(current) {
		if _, err := os.Stat(current); err == nil {
			return current, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		if parent := filepath.Dir(current); parent == current {
			return "", os.ErrNotExist
		}
	}
}

// IsFull reports a write that failed because the disk or the user's quota is full: ENOSPC and EDQUOT, and on
// Windows ERROR_HANDLE_DISK_FULL (39) and ERROR_DISK_FULL (112).
func IsFull(err error) bool {
	return errors.Is(err, syscall.ENOSPC) || errors.Is(err, syscall.EDQUOT) || fullPlatform(err)
}
