package main

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// removeBelowDirectory is removePathPreserving for a process that must not trust the paths: it removes the
// entry name of the open directory parent (path is where that entry is, for the Inbox comparison and check) and
// keeps every preserved Inbox. Nothing is resolved by path: os.Root works relative to the open directory. A file
// or a link is removed as one entry and never followed. A directory is opened once, and check must accept that
// open directory before anything in it is touched; its entries are then removed the same way.
func removeBelowDirectory(parent *os.Root, name, path string, preserved []string, check func(directory *os.Root, path string) error) error {
	holdsInbox := false
	for _, keep := range preserved {
		keep = filepath.Clean(keep)
		if pathWithinOrEqual(keep, path) {
			return nil // the Inbox, or something in it
		}
		holdsInbox = holdsInbox || pathWithinOrEqual(path, keep)
	}
	info, err := parent.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.IsDir() {
		// A directory swapped for a link after the Lstat is opened through the link; check refuses it.
		if err := emptyCheckedDirectory(parent, name, path, preserved, check); err != nil || holdsInbox {
			return err
		}
	}
	if err := parent.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func emptyCheckedDirectory(parent *os.Root, name, path string, preserved []string, check func(directory *os.Root, path string) error) (result error) {
	directory, err := parent.OpenRoot(name)
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
