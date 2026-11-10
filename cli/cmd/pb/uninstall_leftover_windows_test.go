//go:build windows

package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

func TestOnlyAnEmptyLockedBridgeRequestDirectoryIsTolerated(t *testing.T) {
	base := filepath.Join(t.TempDir(), "elevation")
	empty := filepath.Join(base, "request-0123abcd")
	full := filepath.Join(base, "request-4567ef01")
	for _, directory := range []string{empty, full} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(full, "request.json"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	locked := func(path string) error { return &fs.PathError{Op: "unlinkat", Path: path, Err: windows.ERROR_SHARING_VIOLATION} }
	if !leftoverLockedBridgeDirectory(locked(empty)) {
		t.Fatal("an empty locked request directory failed the removal")
	}
	if leftoverLockedBridgeDirectory(locked(full)) {
		t.Fatal("a locked request directory with content was tolerated")
	}
	if leftoverLockedBridgeDirectory(locked(filepath.Join(base, "other"))) {
		t.Fatal("a locked directory that is not a request directory was tolerated")
	}
	denied := &fs.PathError{Op: "unlinkat", Path: empty, Err: windows.ERROR_ACCESS_DENIED}
	if leftoverLockedBridgeDirectory(denied) {
		t.Fatal("a refusal other than a sharing violation was tolerated")
	}
}
