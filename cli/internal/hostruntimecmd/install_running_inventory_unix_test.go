//go:build darwin || linux

package hostruntimecmd

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/buildinfo"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
	"github.com/pinksaucepasta/paperboat/internal/shorttemp"
)

// CI 42029070896: a cancelled pb install still started `sudo pb __runtime-service inventory` under
// context.WithoutCancel, with no deadline, and the workspace test hung for 30 minutes. A cancelled install
// starts no privileged child; a failed one runs the inventory under its own bound.
func TestFailedInstallInventoryIsBoundedAndCancelledInstallStartsNone(t *testing.T) {
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
