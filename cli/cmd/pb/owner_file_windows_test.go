//go:build windows

package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

func TestOwnerOnlyWindowsAttributesPreserveOriginalFailureAndRecover(t *testing.T) {
	path := filepath.Join(t.TempDir(), "protected-input")
	create := func() {
		t.Helper()
		file, err := createEnvironmentRecoveryFile(path)
		if err != nil {
			t.Fatal("protected fixture creation failed")
		}
		if err := file.Close(); err != nil {
			t.Fatal("protected fixture close failed")
		}
	}
	create()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal("protected fixture metadata failed")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal("owned fixture removal failed")
	}
	// The metadata was valid when borrowed. A subsequent filesystem failure
	// belongs to GetFileAttributes and must retain the actual native cause.
	err = validateOwnerOnlyRegularFile(path, info)
	if !errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, errOwnerOnlyFileInvalid) {
		t.Fatal("native attribute failure became a metadata rejection or lost original cause")
	}
	create()
	info, err = os.Lstat(path)
	if err != nil {
		t.Fatal("replacement fixture metadata failed")
	}
	if err := validateOwnerOnlyRegularFile(path, info); err != nil {
		t.Fatal("protected replacement did not recover")
	}
	data, err := readOwnerOnlyFile(path, 8)
	if err != nil || len(data) != 0 {
		t.Fatal("replacement could not be read")
	}
}
