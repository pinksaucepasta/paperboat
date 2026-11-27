//go:build linux || darwin

package hostruntimecmd

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/service"
)

// On Linux, sudo-rs kills a root step about 3 s after its terminal closes even with every signal ignored, and a
// killed sudo hung it up mid-rollback (device checks 11-27). The install, commit and rollback steps run as a
// transient systemd service instead, which finishes whatever happens to pb, sudo or the terminal; other steps and
// macOS keep the plain sudo child.
func TestInstallStepsRunAsATransientServiceOnLinux(t *testing.T) {
	fake := filepath.Join(t.TempDir(), "systemd-run")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	previous, previousSupported := systemdRun, systemdRunSupported
	t.Cleanup(func() { systemdRun, systemdRunSupported = previous, previousSupported })
	systemdRun, systemdRunSupported = fake, func() bool { return true }
	for _, step := range []string{"install-supplied", "commit-supplied", "rollback-supplied", "inventory"} {
		command := privilegedInstallCommand(context.Background(), 1000, "/root", "1000", "/usr/local/bin/pb", "__runtime-service", step)
		detached := slices.Contains(command.Args, fake)
		want := runtime.GOOS == "linux" && step != "inventory"
		if detached != want {
			t.Fatalf("%s on %s: transient service = %v, want %v (%v)", step, runtime.GOOS, detached, want, command.Args)
		}
		if detached {
			for _, flag := range []string{"--wait", "--pipe", "--collect", "--property=KillMode=process", "--unit=paperboat-" + step + "-u1000.service", "HOME=/root"} {
				if !slices.Contains(command.Args, flag) {
					t.Fatalf("%s: launcher lacks %s: %v", step, flag, command.Args)
				}
			}
		}
	}
}

// When systemd-run ends but the step's service runs on (Ctrl-C ended systemd-run, or the rollback bound did), pb must
// not restore or inventory over it: it says the restore is still running and leaves it the installation.
func TestARollbackServiceStillRunningIsLeftToFinish(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the transient service is Linux only")
	}
	executable, source, home := suppliedInstallFixture(t)
	layout, err := service.UserLayout(runtime.GOOS, os.Getuid())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(layout.Binary); err == nil {
		t.Skip("a Paperboat installation exists on this machine; the test must not touch its services")
	}
	fake := filepath.Join(t.TempDir(), "systemd-run")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	previous, previousSupported, previousRunning := systemdRun, systemdRunSupported, detachedStepRunning
	t.Cleanup(func() {
		systemdRun, systemdRunSupported, detachedStepRunning = previous, previousSupported, previousRunning
	})
	systemdRun, systemdRunSupported = fake, func() bool { return true }
	detachedStepRunning = func(unit string) bool { return strings.Contains(unit, "rollback-supplied") }
	original := privilegedInstallCommand
	t.Cleanup(func() { privilegedInstallCommand = original })
	var operations []string
	privilegedInstallCommand = func(ctx context.Context, _ int, _, _, _ string, arguments ...string) *exec.Cmd {
		operation := arguments[len(arguments)-1]
		operations = append(operations, operation)
		if operation == "rollback-supplied" {
			return exec.CommandContext(ctx, "/bin/sh", "-c", "cat >/dev/null; exit 1") // systemd-run interrupted
		}
		return exec.CommandContext(ctx, "/bin/sh", "-c", "cat >/dev/null")
	}
	_, err = InstallRunningBinary(t.Context(), executable, source, filepath.Join(home, "bin"), "")
	var failure *CommandFailure
	if !errors.As(err, &failure) || !failure.StillFinishing() {
		t.Fatalf("a rollback still running as a service = %v, want a still-finishing failure", err)
	}
	if want := []string{"install-supplied", "rollback-supplied"}; !slices.Equal(operations, want) {
		t.Fatalf("privileged steps = %v, want nothing over the running rollback %v", operations, want)
	}
}
