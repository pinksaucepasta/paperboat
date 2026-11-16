//go:build darwin || linux

package hostruntimecmd

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func refusingSudo(t *testing.T, diagnostic string) {
	t.Helper()
	fake := filepath.Join(t.TempDir(), "sudo")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\necho '"+diagnostic+"' >&2\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	previous, previousRoot := sudoExecutable, runningAsRoot
	sudoExecutable, runningAsRoot = fake, func() bool { return false }
	t.Cleanup(func() { sudoExecutable, runningAsRoot = previous, previousRoot })
}

func requireAdministratorFailure(t *testing.T, err error, step string) {
	t.Helper()
	var refused *AdministratorRequiredError
	text := ""
	if err != nil {
		text = err.Error()
	}
	if !errors.As(err, &refused) || refused.Step != step || !strings.Contains(text, "sudo") || !strings.Contains(text, step) ||
		!strings.Contains(text, "usermod") && !strings.Contains(text, "System Settings") || strings.Contains(text, "invalid result") {
		t.Fatalf("a sudo refusal must name the step %q, sudo and how to grant it: %v", step, err)
	}
}

func TestSudoRefusalNamesTheStepForEveryKindOfRefusal(t *testing.T) {
	failed := errors.New("exit status 1")
	for _, stderr := range []string{
		"sudo: a password is required",
		"sudo: a terminal is required to read the password; either use the -S option",
		"pbt2 is not in the sudoers file.",
		"Sorry, user pbt2 may not run sudo on host.",
		"sudo: 3 incorrect password attempts",
	} {
		requireAdministratorFailure(t, sudoRefusal("Doing the step", failed, stderr), "Doing the step")
	}
	missing := &exec.Error{Name: "/usr/bin/sudo", Err: exec.ErrNotFound}
	if err := sudoRefusal("Doing the step", &exec.Error{Name: "sudo", Err: os.ErrNotExist}, ""); err == nil || !strings.Contains(err.Error(), "sudo is not installed") {
		t.Fatalf("missing sudo binary: %v (%v)", err, missing)
	}
	if sudoRefusal("Doing the step", failed, "mkdir: cannot create directory: No space left on device") != nil || sudoRefusal("Doing the step", nil, "sudo: a password is required") != nil {
		t.Fatal("only a failed sudo is a refusal")
	}
}

// A user without sudo rights saw "privileged cleanup returned an invalid result" (FLEET4, pbt2 on pbvm2): the install
// inventory read sudo's empty stdout as a broken report.
func TestInventoryUnderARefusedSudoNamesSudoNotAnInvalidResult(t *testing.T) {
	command := exec.CommandContext(t.Context(), "/bin/sh", "-c", "echo 'pbt2 is not in the sudoers file.' >&2; exit 1")
	err := runInventoryCommand(t.Context(), command, "/bin/true")
	requireAdministratorFailure(t, err, "Cleaning up other Paperboat versions (the install inventory)")
}

func TestPurgeUnderARefusedSudoNamesSudo(t *testing.T) {
	refusingSudo(t, "sudo: a password is required")
	_, err := runPurge(t.Context(), nil, nil, nil, nil)
	requireAdministratorFailure(t, err, "Removing the Paperboat system runtime")
}

func TestPersistedUninstallAndRepairUnderARefusedSudoNameSudo(t *testing.T) {
	refusingSudo(t, "sudo: a terminal is required")
	requireAdministratorFailure(t, authorizePersistedUninstall(t.Context()), "Removing the Paperboat system service")
	requireAdministratorFailure(t, RepairManagedHost(t.Context()), "Repairing the Paperboat system service")
}

func TestInstallUnderARefusedSudoReportsOneSudoSentence(t *testing.T) {
	executable, source, home := suppliedInstallFixture(t)
	original := privilegedInstallCommand
	t.Cleanup(func() { privilegedInstallCommand = original })
	privilegedInstallCommand = func(ctx context.Context, _ int, _, _, _ string, _ ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "/bin/sh", "-c", "echo 'sudo: a password is required' >&2; exit 1")
	}
	_, err := InstallRunningBinary(t.Context(), executable, source, filepath.Join(home, "bin"), "")
	requireAdministratorFailure(t, err, "Installing the supplied Paperboat executable")
	if strings.Count(err.Error(), "needs administrator rights") != 1 {
		t.Fatalf("the install and its inventory both refused, one sentence expected: %v", err)
	}
}
