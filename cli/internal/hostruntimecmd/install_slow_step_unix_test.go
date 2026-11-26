//go:build darwin || linux

package hostruntimecmd

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/hostinstall"
	"github.com/pinksaucepasta/paperboat/cli/internal/localdaemon"
)

// dadape, 2026-11-26: the privileged install step (idle I/O on a busy disk) ran past pb install's 3 min deadline;
// killing sudo left the root step finishing the install while pb reported "installing ... failed". A slow
// privileged step now runs to its end and the install reports what happened after it, not a deadline failure.
func TestASlowPrivilegedInstallStepIsNotReportedAsFailed(t *testing.T) {
	executable, source, home := suppliedInstallFixture(t)
	original := privilegedInstallCommand
	t.Cleanup(func() { privilegedInstallCommand = original })
	finished := filepath.Join(home, "install-supplied-finished")
	var operations []string
	privilegedInstallCommand = func(ctx context.Context, _ int, _, _, _ string, arguments ...string) *exec.Cmd {
		operations = append(operations, arguments[len(arguments)-1])
		if arguments[len(arguments)-1] == "install-supplied" {
			return exec.CommandContext(ctx, "/bin/sh", "-c", "sleep 0.5 && touch "+finished)
		}
		return exec.CommandContext(ctx, "/bin/false")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	_, err := InstallRunningBinary(ctx, executable, source, filepath.Join(home, "bin"), "")
	if _, statErr := os.Stat(finished); statErr != nil {
		t.Fatalf("the privileged step was killed at the caller's deadline: %v (install error: %v)", statErr, err)
	}
	// The install went on past the finished step with a budget of its own (this fixture's later steps fail and
	// roll back); before, the killed step ended the install with only the inventory after it.
	if len(operations) < 2 || operations[1] == "inventory" {
		t.Fatalf("install stopped at the slow step: privileged operations %v (%v)", operations, err)
	}
}

// Past the outer bound pb stops waiting: the root child is still finishing, so the message says so instead of
// "failed".
func TestAPrivilegedStepPastItsBoundSaysStillFinishing(t *testing.T) {
	executable, source, home := suppliedInstallFixture(t)
	originalCommand, originalBound := privilegedInstallCommand, suppliedInstallBound
	t.Cleanup(func() { privilegedInstallCommand, suppliedInstallBound = originalCommand, originalBound })
	suppliedInstallBound = 100 * time.Millisecond
	privilegedInstallCommand = func(ctx context.Context, _ int, _, _, _ string, arguments ...string) *exec.Cmd {
		if arguments[len(arguments)-1] == "install-supplied" {
			return exec.CommandContext(ctx, "/bin/sleep", "5")
		}
		return exec.CommandContext(ctx, "/bin/false")
	}
	_, err := InstallRunningBinary(t.Context(), executable, source, filepath.Join(home, "bin"), "")
	message := runtimeFailureMessage(err)
	if !strings.Contains(message, "still finishing in the background") || strings.Contains(message, "failed") {
		t.Fatalf("a step past its bound said %q (%v)", message, err)
	}
}

// A second pb install while one runs names the other install instead of a generic failure.
func TestASecondInstallSaysAnotherInstallIsRunning(t *testing.T) {
	executable, source, home := suppliedInstallFixture(t)
	paths, err := localdaemon.CurrentUserPaths()
	if err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(paths.StateRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	held, err := hostinstall.LockSuppliedOperation(filepath.Join(paths.StateRoot, "install.lock"), os.Getuid())
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	_, err = InstallRunningBinary(t.Context(), executable, source, filepath.Join(home, "bin"), "")
	if !strings.Contains(runtimeFailureMessage(err), "another Paperboat install or update is running") {
		t.Fatalf("second install said %q (%v)", runtimeFailureMessage(err), err)
	}
}
