//go:build windows

package service

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestOwnedWindowsExecutableDistinguishesAbsentDeclaration(t *testing.T) {
	root := t.TempDir()
	for _, path := range []string{
		filepath.Join(root, "missing.json"),
		filepath.Join(root, "missing-parent", "missing.json"),
	} {
		_, err := OwnedWindowsExecutable(path)
		if !errors.Is(err, os.ErrNotExist) || errors.Is(err, ErrInvalidDefinition) {
			t.Fatalf("absent declaration must permit lifecycle recovery: %v", err)
		}
	}
	malformed := filepath.Join(root, "malformed.json")
	if err := os.WriteFile(malformed, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OwnedWindowsExecutable(malformed); !errors.Is(err, ErrInvalidDefinition) || errors.Is(err, os.ErrNotExist) {
		t.Fatalf("present malformed declaration must fail closed: %v", err)
	}
	// A regular file cannot be a declaration's parent. This unsafe ancestor
	// must remain invalid even though the requested child does not exist.
	if _, err := OwnedWindowsExecutable(filepath.Join(malformed, "missing.json")); !errors.Is(err, ErrInvalidDefinition) {
		t.Fatalf("unsafe ancestor must fail closed: %v", err)
	}
}
