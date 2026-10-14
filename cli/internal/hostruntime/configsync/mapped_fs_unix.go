//go:build unix

package configsync

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// Pin each verified directory through openat(O_NOFOLLOW), including when
// creating parents. Subsequent IO stays on that descriptor if names are swapped.
func openMappedParent(target string, create bool) (int, error) {
	current, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, err
	}
	for _, part := range strings.Split(strings.TrimPrefix(filepath.Dir(target), "/"), "/") {
		if part == "" {
			continue
		}
		next, openErr := unix.Openat(current, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if errors.Is(openErr, unix.ENOENT) && create {
			if err := unix.Mkdirat(current, part, 0700); err != nil && !errors.Is(err, unix.EEXIST) {
				unix.Close(current)
				return -1, err
			}
			next, openErr = unix.Openat(current, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		}
		unix.Close(current)
		if openErr != nil {
			return -1, openErr
		}
		current = next
	}
	return current, nil
}
func secureReadFile(target string, max int64) ([]byte, os.FileInfo, error) {
	parent, err := openMappedParent(target, false)
	if err != nil {
		return nil, nil, err
	}
	defer unix.Close(parent)
	fd, err := unix.Openat(parent, filepath.Base(target), unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, nil, err
	}
	file := os.NewFile(uintptr(fd), target)
	defer file.Close()
	before, err := file.Stat()
	if err != nil {
		return nil, nil, err
	}
	if !before.Mode().IsRegular() || before.Size() > max {
		return nil, nil, ErrSnapshotInvalid
	}
	data, err := io.ReadAll(io.LimitReader(file, max+1))
	if err != nil {
		return nil, nil, err
	}
	after, err := file.Stat()
	if err != nil {
		return nil, nil, err
	}
	if int64(len(data)) > max || !os.SameFile(before, after) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return nil, nil, ErrSourceChanged
	}
	return data, after, nil
}
func secureWriteFile(target string, value []byte, mode os.FileMode) error {
	parent, err := openMappedParent(target, true)
	if err != nil {
		return err
	}
	defer unix.Close(parent)
	var stat unix.Stat_t
	if err := unix.Fstatat(parent, filepath.Base(target), &stat, unix.AT_SYMLINK_NOFOLLOW); err == nil {
		if stat.Mode&unix.S_IFMT != unix.S_IFREG {
			return ErrPathRuleInvalid
		}
		mode = os.FileMode(stat.Mode & 0777)
		if stat.Mode&0200 == 0 {
			return &PathRuleError{Code: "unwritable_destination"}
		}
	} else if !errors.Is(err, unix.ENOENT) {
		return err
	}
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	name := ".paperboat-config-" + hex.EncodeToString(nonce)
	fd, err := unix.Openat(parent, name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, uint32(mode.Perm()))
	if err != nil {
		return err
	}
	defer unix.Unlinkat(parent, name, 0)
	file := os.NewFile(uintptr(fd), name)
	_, writeErr := file.Write(value)
	syncErr := file.Sync()
	closeErr := file.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		return err
	}
	if err := unix.Renameat(parent, name, parent, filepath.Base(target)); err != nil {
		return err
	}
	return unix.Fsync(parent)
}
func secureRemoveFile(target string) error {
	parent, err := openMappedParent(target, false)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil {
		return err
	}
	defer unix.Close(parent)
	var stat unix.Stat_t
	err = unix.Fstatat(parent, filepath.Base(target), &stat, unix.AT_SYMLINK_NOFOLLOW)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil {
		return err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return ErrPathRuleInvalid
	}
	if err := unix.Unlinkat(parent, filepath.Base(target), 0); err != nil {
		return err
	}
	return unix.Fsync(parent)
}

func checkMappedPlatformPath(string) error { return nil }
