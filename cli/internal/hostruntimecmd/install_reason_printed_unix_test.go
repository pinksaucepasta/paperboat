//go:build darwin || linux

package hostruntimecmd

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// "its reason is printed above" is said only when the privileged step wrote something: a silent or never-started
// step left nothing above it.
func TestInstallFailurePointsAboveOnlyWhenTheStepPrintedAReason(t *testing.T) {
	executable, source, home := suppliedInstallFixture(t)
	original := privilegedInstallCommand
	t.Cleanup(func() { privilegedInstallCommand = original })
	script := ""
	privilegedInstallCommand = func(ctx context.Context, _ int, _, _, _ string, _ ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "/bin/sh", "-c", script)
	}
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	for _, c := range []struct {
		name, script string
		ctx          context.Context
		above        bool
	}{
		{"silent step", "exit 1", t.Context(), false},
		{"blank stderr", "echo >&2; exit 1", t.Context(), false},
		{"step with a reason", "echo 'pb: disk is read-only' >&2; exit 1", t.Context(), true},
		{"never started", "exit 1", cancelled, false},
	} {
		script = c.script
		_, err := InstallRunningBinary(c.ctx, executable, source, filepath.Join(home, "bin"), "")
		var failure *CommandFailure
		if !errors.As(err, &failure) || !strings.HasPrefix(failure.Error(), "installing the supplied Paperboat executable failed") || !strings.Contains(failure.Error(), "rerun `pb install`") {
			t.Fatalf("%s: err = %v", c.name, err)
		}
		if strings.Contains(failure.Error(), "printed above") != c.above {
			t.Fatalf("%s: message = %q, want it to point above: %v", c.name, failure.Error(), c.above)
		}
	}
}
