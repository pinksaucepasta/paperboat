//go:build !windows

package configsync

import (
	"errors"
	"os"
)

func syncRepositoryObject(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(file.Sync(), file.Close())
}

func syncRepositoryDirectory(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	return file.Sync()
}
func renameRepositoryReference(source, target string) error { return os.Rename(source, target) }
