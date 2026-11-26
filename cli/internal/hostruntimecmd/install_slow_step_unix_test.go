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
	privilegedInstallCommand = func(ctx context.Context, _ int, _, _, _ string, arguments ...string) *exec.Cmd {
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
	if err != nil && strings.Contains(runtimeFailureMessage(err), "installing the supplied Paperboat executable failed") {
		t.Fatalf("a privileged step that finished was reported as failed: %v", err)
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
