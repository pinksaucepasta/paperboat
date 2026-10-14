//go:build !unix

package configsync

import (
	"errors"
	"io"
	"os"
	"path/filepath"
)

func secureReadFile(target string, max int64) ([]byte, os.FileInfo, error) {
	if err := checkSafeAbsolutePath(target); err != nil {
		return nil, nil, err
	}
	release, err := lockMappedParents(target, false)
	if err != nil {
		return nil, nil, err
	}
	defer release()
	root, err := os.OpenRoot(filepath.Dir(target))
	if err != nil {
		return nil, nil, err
	}
	defer root.Close()
	file, err := root.Open(filepath.Base(target))
	if err != nil {
		return nil, nil, err
	}
	defer file.Close()
	before, err := file.Stat()
	if err != nil {
		return nil, nil, err
	}
	if !before.Mode().IsRegular() || before.Size() > max {
		return nil, nil, ErrSnapshotInvalid
	}
	value, err := io.ReadAll(io.LimitReader(file, max+1))
	if err != nil {
		return nil, nil, err
	}
	after, err := file.Stat()
	if err != nil {
		return nil, nil, err
	}
	if int64(len(value)) > max || !os.SameFile(before, after) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return nil, nil, ErrSourceChanged
	}
	return value, after, nil
}
func secureWriteFile(target string, value []byte, mode os.FileMode) error {
	if err := checkSafeAbsolutePath(target); err != nil {
		return err
	}
	release, err := lockMappedParents(target, true)
	if err != nil {
		return err
	}
	defer release()
	return restoreRegularFile(target, value, mode.Perm())
}
func secureRemoveFile(target string) error {
	if err := checkSafeAbsolutePath(target); err != nil {
		return err
	}
	release, err := lockMappedParents(target, false)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer release()
	root, err := os.OpenRoot(filepath.Dir(target))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer root.Close()
	info, err := root.Lstat(filepath.Base(target))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return ErrPathRuleInvalid
	}
	return root.Remove(filepath.Base(target))
}
