package main

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// An unexpected failure's cause text is kept locally (owner-only), so a bare internal error can be traced.
func TestUnexpectedFailureCauseIsKeptLocallyOwnerOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("owner-only is an ACL on Windows, checked by atomicfile")
	}
	directory := filepath.Join(t.TempDir(), "diagnostics")
	cause := errors.Join(errors.New("committed transfer offset did not advance"), errors.New("second leaf"))
	if err := recordUnexpectedFailureCause(directory, "send", "support_ref", cause); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, lastUnexpectedFailureFile)
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0o600 || !strings.Contains(string(body), "send support_ref") || !strings.Contains(string(body), "committed transfer offset did not advance") || !strings.Contains(string(body), "second leaf") {
		t.Fatalf("mode %v body %q", info.Mode().Perm(), body)
	}
}
