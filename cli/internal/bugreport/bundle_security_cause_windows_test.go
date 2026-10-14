//go:build windows

package bugreport

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"golang.org/x/sys/windows"
)

func TestBundleSecurityWindowsProbeCauseAndProtectedRecovery(t *testing.T) {
	path := filepath.Join(t.TempDir(), "PRIVATE_BUNDLE")
	if err := writeTestBundle(path, []byte("redacted bundle")); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if safe, err := checkBundleFile(path, info); err != nil || !safe {
		t.Fatal("protected bundle rejected")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	safe, err := checkBundleFile(path, info)
	if safe || !errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
		t.Fatal("disappeared native probe lost original cause")
	}
	safe, err = checkBundleFile("invalid\x00path", info)
	if safe || !errors.Is(err, syscall.EINVAL) {
		t.Fatal("invalid native path lost original cause")
	}
	if err := writeTestBundle(path, []byte("redacted bundle")); err != nil {
		t.Fatal(err)
	}
	info, err = os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if safe, err := checkBundleFile(path, info); err != nil || !safe {
		t.Fatal("fresh protected bundle did not recover")
	}
	directory := t.TempDir()
	directoryInfo, err := os.Lstat(directory)
	if err != nil {
		t.Fatal(err)
	}
	if safe, err := checkBundleFile(directory, directoryInfo); safe || err != nil {
		t.Fatal("directory accepted")
	}
	local := &localStub{bundle: testBundle(t, []byte("redacted bundle"))}
	if result, err := Run(t.Context(), Options{Input: bytes.NewReader(nil), Prompt: io.Discard, Local: local}); err != nil || result.Validate() != nil {
		t.Fatal("protected workflow did not complete")
	}
}
