//go:build !windows

package service

import (
	"os"
	"slices"
	"strconv"
	"testing"
)

func TestUserSystemdControllerSuppliesRuntimeDir(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "")
	uid := os.Getuid()
	dir := "/run/user/" + strconv.Itoa(uid)
	runner := UserSystemdController(ExecRunner{}, "paperboatd.service", uid).Runner.(ExecRunner)
	if ownRuntimeDir(dir, uid) != slices.Contains(runner.Environment, "XDG_RUNTIME_DIR="+dir) {
		t.Fatalf("runtime dir %s (own %v) not handled: %v", dir, ownRuntimeDir(dir, uid), runner.Environment)
	}
	// Another account's or a missing directory is never handed to systemctl.
	if runner := UserSystemdController(ExecRunner{}, "paperboatd.service", 424242).Runner.(ExecRunner); runner.Environment != nil {
		t.Fatalf("unowned runtime dir supplied: %v", runner.Environment)
	}
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/1000")
	if runner := UserSystemdController(ExecRunner{}, "paperboatd.service", 1000).Runner.(ExecRunner); runner.Environment != nil {
		t.Fatalf("an existing runtime dir must be inherited unchanged: %v", runner.Environment)
	}
}
