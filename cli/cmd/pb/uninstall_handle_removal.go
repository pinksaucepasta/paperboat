package main

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/pinksaucepasta/paperboat/cli/internal/ownedpaths"
)

// removeBelowDirectory is removeOwned for a process that must not trust the paths: it removes the entry name of the
// open directory parent (path is where that entry is, for the Inbox comparison and check), keeps every preserved
// Inbox and, of a directory, everything not in owned (nil: all of it is owned). Nothing is resolved by path: os.Root
// works relative to the open directory. A file or a link is removed as one entry and never followed. A directory is
// opened once, and check must accept that open directory before anything in it is touched; its entries are then
// removed the same way. kept reports that something was left on purpose.
func removeBelowDirectory(parent *os.Root, name, path string, preserved []string, owned ownedpaths.Tree, check func(directory *os.Root, path string) error) (kept bool, result error) {
	for _, keep := range preserved {
		keep = filepath.Clean(keep)
		if pathWithinOrEqual(keep, path) {
			return true, nil // the Inbox, or something in it
		}
		kept = kept || pathWithinOrEqual(path, keep)
	}
	info, err := parent.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if info.IsDir() {
		// A directory swapped for a link after the Lstat is opened through the link; check refuses it.
		entriesKept, err := emptyCheckedDirectory(parent, name, path, preserved, owned, check)
		kept = kept || entriesKept
		if err != nil || kept {
			return kept, err
		}
	}
	if err := parent.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	return false, nil
}

func emptyCheckedDirectory(parent *os.Root, name, path string, preserved []string, owned ownedpaths.Tree, check func(directory *os.Root, path string) error) (kept bool, result error) {
	directory, err := parent.OpenRoot(name)
	if err != nil {
		return false, err
	}
	defer func() { result = errors.Join(result, directory.Close()) }()
	if err := check(directory, path); err != nil {
		return false, err
	}
	entries, err := fs.ReadDir(directory.FS(), ".")
	if err != nil {
		return false, err
	}
	for _, entry := range entries {
		child, ok := owned.Child(entry.Name())
		if !ok {
			kept = true
			continue
		}
		childKept, err := removeBelowDirectory(directory, entry.Name(), filepath.Join(path, entry.Name()), preserved, child, check)
		kept = kept || childKept
		result = errors.Join(result, err)
	}
	return kept, result
}
