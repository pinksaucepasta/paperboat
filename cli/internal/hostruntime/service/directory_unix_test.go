//go:build !windows

package service

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// A unit directory under a parent any account can write to could be swapped for a link to somewhere else.
func TestPrepareAtomicDirectoryRefusesAWritableAncestor(t *testing.T) {
	root := t.TempDir()
	if err := prepareAtomicDirectory(filepath.Join(root, "units")); err != nil {
		t.Fatalf("a private parent was refused: %v", err)
	}
	open := filepath.Join(root, "open")
	if err := os.Mkdir(open, 0o777); err != nil || os.Chmod(open, 0o777) != nil {
		t.Fatal(err)
	}
	if err := prepareAtomicDirectory(filepath.Join(open, "units")); !errors.Is(err, ErrInvalidDefinition) {
		t.Fatalf("a world-writable parent = %v, want ErrInvalidDefinition", err)
	}
}
