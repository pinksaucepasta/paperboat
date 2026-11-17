//go:build darwin || linux

package hostruntimecmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// a test VM (Ubuntu 24.04, passwordless sudo) failed `pb pair`: its sudoers has a
// stock `(ALL:ALL) ALL` rule before `NOPASSWD: ALL`, so `sudo -n true` works but
// `sudo -v` still demands a password.
func TestApproveAdministratorSkipsApprovalWhenSudoIsPasswordless(t *testing.T) {
	fake := filepath.Join(t.TempDir(), "sudo")
	script := "#!/bin/sh\n[ \"$1\" = -n ] && exit 0\necho 'sudo: a password is required' >&2\nexit 1\n"
	if err := os.WriteFile(fake, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	previous, previousRoot := sudoExecutable, runningAsRoot
	sudoExecutable, runningAsRoot = fake, func() bool { return false }
	t.Cleanup(func() { sudoExecutable, runningAsRoot = previous, previousRoot })
	var stderr bytes.Buffer
	if err := approveAdministrator(t.Context(), bytes.NewReader(nil), &stderr); err != nil || stderr.Len() != 0 {
		t.Fatalf("passwordless sudo must not require approval: err=%v stderr=%q", err, stderr.String())
	}
}

func TestApproveAdministratorStillRequiresApprovalWhenSudoNeedsAPassword(t *testing.T) {
	fake := filepath.Join(t.TempDir(), "sudo")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	previous, previousRoot := sudoExecutable, runningAsRoot
	sudoExecutable, runningAsRoot = fake, func() bool { return false }
	t.Cleanup(func() { sudoExecutable, runningAsRoot = previous, previousRoot })
	err := approveAdministrator(t.Context(), bytes.NewReader(nil), &bytes.Buffer{})
	if err == nil {
		t.Fatal("a refused sudo must fail administrator approval")
	}
	// A pair without a terminal must say it needs sudo, not "installing system services".
	message := (&installationStageError{Stage: "service_install", Cause: err}).Error()
	if !strings.Contains(message, "administrator approval (sudo)") || !strings.Contains(message, "passwordless sudo") {
		t.Fatalf("pairing message does not name the sudo step: %q", message)
	}
}

// F26: a recipe install never asks for a password: without root or passwordless sudo it stops at once
// with what to do, and root runs privileged steps without sudo (minimal images have none).
func TestRecipeInstallNeverPromptsAndRootSkipsSudo(t *testing.T) {
	fake := filepath.Join(t.TempDir(), "sudo")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\n[ \"$1\" = -n ] && exit 1\necho prompted >&2\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	previous, previousRoot := sudoExecutable, runningAsRoot
	sudoExecutable, runningAsRoot = fake, func() bool { return false }
	t.Cleanup(func() { sudoExecutable, runningAsRoot = previous, previousRoot })
	t.Setenv("PB_RECIPE", "pbr_x")
	var stderr bytes.Buffer
	err := approveAdministrator(t.Context(), bytes.NewReader(nil), &stderr)
	if err == nil || !strings.Contains(err.Error(), "root or passwordless sudo") || strings.Contains(stderr.String(), "prompted") {
		t.Fatalf("err=%v stderr=%q", err, stderr.String())
	}
	// The user sees the recipe text, not the interactive "run pair from a terminal" one (device check, rcp-l8).
	if message := (&installationStageError{Stage: "service_install", Cause: err}).Error(); message != recipeNeedsRootMessage {
		t.Fatalf("pairing message = %q", message)
	}
	runningAsRoot = func() bool { return true }
	if err := approveAdministrator(t.Context(), bytes.NewReader(nil), &stderr); err != nil {
		t.Fatalf("root needs no approval: %v", err)
	}
	if command := sudoCommand(t.Context(), "--", "/usr/bin/env", "X=1"); command.Path != "/usr/bin/env" || strings.Join(command.Args[1:], " ") != "X=1" {
		t.Fatalf("root ran %v", command.Args)
	}
}
