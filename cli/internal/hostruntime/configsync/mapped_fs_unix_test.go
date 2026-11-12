//go:build unix

package configsync

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// A service runs with umask 077; replacing a mapped file must keep the destination's own permission bits (X-04).
func TestSecureWriteFileKeepsTheBitsOfAnExistingDestinationUnderAStrictUmask(t *testing.T) {
	old := syscall.Umask(0o077)
	defer syscall.Umask(old)
	dir := resolvedTempDir(t)
	target := filepath.Join(dir, "settings.txt")
	writeTestFile(t, target, "v1")
	if err := os.Chmod(target, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := secureWriteFile(target, []byte("v2"), 0o600); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(target); err != nil || info.Mode().Perm() != 0o640 {
		t.Fatalf("replacement changed the mode: %v %v", info, err)
	}
	fresh := filepath.Join(dir, "run.txt")
	if err := secureWriteFile(fresh, []byte("#!/bin/sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(fresh); err != nil || info.Mode().Perm() != 0o755 {
		t.Fatalf("a new executable file lost its bits under umask 077: %v %v", info, err)
	}
}
