//go:build darwin || linux

package hostruntimecmd

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/buildinfo"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostinstall"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
	"github.com/pinksaucepasta/paperboat/internal/shorttemp"
)

// CI 42029070896: a cancelled pb install still started `sudo pb __runtime-service inventory` under
// context.WithoutCancel, with no deadline, and the workspace test hung for 30 minutes. A cancelled install
// starts no privileged child; a failed one runs the inventory under its own bound.
func TestFailedInstallInventoryIsBoundedAndCancelledInstallStartsNone(t *testing.T) {
	executable, source, home := suppliedInstallFixture(t)
	original := privilegedInstallCommand
	t.Cleanup(func() { privilegedInstallCommand = original })
	type started struct {
		operation string
		bounded   bool
	}
	var children []started
	privilegedInstallCommand = func(ctx context.Context, _ int, _, _, _ string, arguments ...string) *exec.Cmd {
		deadline, bounded := ctx.Deadline()
		children = append(children, started{arguments[len(arguments)-1], bounded && time.Until(deadline) <= installInventoryTimeout})
		return exec.CommandContext(ctx, "/bin/false") // every privileged step fails
	}

	ctx, cancel := context.WithCancelCause(t.Context())
	cancel(errors.New("cancelled by the caller"))
	if _, err := InstallRunningBinary(ctx, executable, source, filepath.Join(home, "bin"), ""); err == nil {
		t.Fatal("cancelled install succeeded")
	}
	if want := []started{{"install-supplied", false}}; !reflect.DeepEqual(children, want) {
		t.Fatalf("cancelled install asked for %+v, want only the cancelled install step %+v", children, want)
	}

	children = nil
	if _, err := InstallRunningBinary(t.Context(), executable, source, filepath.Join(home, "bin"), ""); err == nil {
		t.Fatal("failed install succeeded")
	}
	if want := []started{{"install-supplied", false}, {"inventory", true}}; !reflect.DeepEqual(children, want) {
		t.Fatalf("failed install started %+v, want the install step then a bounded inventory %+v", children, want)
	}
}

// suppliedInstallFixture is an owner-only copy of the test binary as a supplied pb, in a fresh home.
func suppliedInstallFixture(t *testing.T) (string, installsource.Source, string) {
	t.Helper()
	home := shorttemp.Dir(t)
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	t.Setenv("XDG_RUNTIME_DIR", home)
	current, _, err := installsource.Current()
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(current)
	if err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(home, "pb-copy")
	if err := os.WriteFile(executable, body, 0o700); err != nil {
		t.Fatal(err)
	}
	source, err := installsource.Inspect(executable, buildinfo.Version, buildinfo.Distribution)
	if err != nil {
		t.Fatal(err)
	}
	return executable, source, home
}

// soft-g-11051952: a newer pb downgrading to dev-ab41be003 sent `inventory` to that build's service, which
// predates it; the user saw "privileged cleanup returned an invalid result". The service's usage exit becomes
// a typed error that names the step and the version.
func TestInventoryOnAnOlderServiceNamesTheVersionNotAnInvalidResult(t *testing.T) {
	if code := Execute(t.Context(), []string{"service", "a-verb-from-a-newer-pb"}, strings.NewReader(""), io.Discard, io.Discard); code != ServiceUsageExitCode {
		t.Fatalf("unknown service operation exited %d, want %d", code, ServiceUsageExitCode)
	}
	executable, source, home := suppliedInstallFixture(t)
	originalCommand, originalInspect := privilegedInstallCommand, inspectInventoryExecutable
	t.Cleanup(func() { privilegedInstallCommand, inspectInventoryExecutable = originalCommand, originalInspect })
	privilegedInstallCommand = func(ctx context.Context, _ int, _, _, _ string, arguments ...string) *exec.Cmd {
		if arguments[len(arguments)-1] == "inventory" {
			return exec.CommandContext(ctx, "/bin/sh", "-c", "exit "+strconv.Itoa(ServiceUsageExitCode))
		}
		return exec.CommandContext(ctx, "/bin/false")
	}
	inspectInventoryExecutable = func(context.Context, string) (installsource.Source, error) {
		return installsource.Source{Version: "dev-ab41be003"}, nil
	}
	_, err := InstallRunningBinary(t.Context(), executable, source, filepath.Join(home, "bin"), "")
	var unsupported InventoryUnsupportedError
	if !errors.As(err, &unsupported) || unsupported.Version != "dev-ab41be003" {
		t.Fatalf("err = %v, want InventoryUnsupportedError naming dev-ab41be003", err)
	}
	message := runtimeFailureMessage(err)
	if strings.Contains(err.Error()+message, "invalid result") || !strings.Contains(err.Error(), "Paperboat dev-ab41be003 cannot run the install inventory (it is older than this pb); install it with its own installer") {
		t.Fatalf("failure reads %q / %q", err.Error(), message)
	}
}

// F14-03 (disk full): the privileged step's insufficient-space exit status crosses the elevation boundary as
// hostinstall.ErrInsufficientSpace. The update then goes no further: nothing is committed or rolled back, no
// command link is published, and only the inventory runs.
func TestInstallRefusedForDiskSpaceChangesNothingAndStopsThere(t *testing.T) {
	executable, source, home := suppliedInstallFixture(t)
	original := privilegedInstallCommand
	t.Cleanup(func() { privilegedInstallCommand = original })
	var operations []string
	privilegedInstallCommand = func(ctx context.Context, _ int, _, _, _ string, arguments ...string) *exec.Cmd {
		operation := arguments[len(arguments)-1]
		operations = append(operations, operation)
		if operation == "install-supplied" {
			return exec.CommandContext(ctx, "/bin/sh", "-c", "exit "+strconv.Itoa(InsufficientSpaceExitCode))
		}
		return exec.CommandContext(ctx, "/bin/false")
	}
	directory := filepath.Join(home, "bin")
	_, err := InstallRunningBinary(t.Context(), executable, source, directory, "")
	if !errors.Is(err, hostinstall.ErrInsufficientSpace) {
		t.Fatalf("install on a full disk = %v, want ErrInsufficientSpace", err)
	}
	if want := []string{"install-supplied", "inventory"}; !reflect.DeepEqual(operations, want) {
		t.Fatalf("privileged steps = %v, want %v", operations, want)
	}
	if _, statErr := os.Lstat(filepath.Join(directory, "pb")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("a refused install published the pb command: %v", statErr)
	}
}
