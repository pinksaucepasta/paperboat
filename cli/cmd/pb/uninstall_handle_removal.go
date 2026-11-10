package main

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// removeBelowDirectory is removePathPreserving for a process that must not trust the paths: it removes the
// entry name of the open directory parent (path is where that entry is, for the Inbox comparison only) and
// keeps every preserved Inbox. Nothing is resolved by path and no link is followed: os.Root works relative to
// the open directory and removes a link as an entry. A directory that holds an Inbox is entered; check must
// accept each one before anything in it is touched.
func removeBelowDirectory(parent *os.Root, name, path string, preserved []string, check func(directory *os.Root, path string) error) (result error) {
	holdsInbox := false
	for _, keep := range preserved {
		keep = filepath.Clean(keep)
		if pathWithinOrEqual(keep, path) {
			return nil // the Inbox, or something in it
		}
		holdsInbox = holdsInbox || pathWithinOrEqual(path, keep)
	}
	if !holdsInbox {
		return parent.RemoveAll(name)
	}
	directory, err := parent.OpenRoot(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, directory.Close()) }()
	if err := check(directory, path); err != nil {
		return err
	}
	entries, err := fs.ReadDir(directory.FS(), ".")
	if err != nil {
		return err
	}
	for _, entry := range entries {
		result = errors.Join(result, removeBelowDirectory(directory, entry.Name(), filepath.Join(path, entry.Name()), preserved, check))
	}
	return result
}
