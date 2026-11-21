package service

import (
	"slices"
	"testing"
)

func TestUserSystemdControllerSuppliesRuntimeDir(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "")
	runner := UserSystemdController(ExecRunner{}, "paperboatd.service", 1000).Runner.(ExecRunner)
	if !slices.Contains(runner.Environment, "XDG_RUNTIME_DIR=/run/user/1000") {
		t.Fatalf("no runtime dir for systemctl --user: %v", runner.Environment)
	}
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/1000")
	if runner := UserSystemdController(ExecRunner{}, "paperboatd.service", 1000).Runner.(ExecRunner); runner.Environment != nil {
		t.Fatalf("an existing runtime dir must be inherited unchanged: %v", runner.Environment)
	}
}
