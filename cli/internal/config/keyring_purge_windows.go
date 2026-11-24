//go:build windows

package config

import (
	"errors"
	"os"
	"path/filepath"

	"github.com/pinksaucepasta/paperboat/cli/internal/ownedpaths"
)

func purgeCredentialStore() error {
	directory, err := dpapiCredentialDirectory()
	if err != nil {
		return err
	}
	// Only the secrets: the directory may hold the user's own files, and a link in its place is not followed.
	info, err := os.Lstat(directory)
	if errors.Is(err, os.ErrNotExist) || err == nil && !info.Mode().IsDir() {
		return nil
	}
	entries, readErr := os.ReadDir(directory)
	if err = errors.Join(err, readErr); err != nil {
		return err
	}
	owned := ownedpaths.ForRoot(directory)
	for _, entry := range entries {
		if _, ok := owned.Child(entry.Name()); !ok || filepath.Ext(entry.Name()) != ".dpapi" {
			continue
		}
		if removeErr := os.Remove(filepath.Join(directory, entry.Name())); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			err = errors.Join(err, removeErr)
		}
	}
	return err
}
