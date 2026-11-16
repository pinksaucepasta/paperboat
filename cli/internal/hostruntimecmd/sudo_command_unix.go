//go:build !windows

package hostruntimecmd

import (
	"context"
	"os"
	"os/exec"
)

// sudoCommand starts /usr/bin/sudo with sudoHome's flags first (none on Linux). As root it runs the
// command after "--" directly: root needs no sudo, and minimal container images have none.
func sudoCommand(ctx context.Context, args ...string) *exec.Cmd {
	if runningAsRoot() && len(args) > 1 && args[0] == "--" {
		return exec.CommandContext(ctx, args[1], args[2:]...)
	}
	return exec.CommandContext(ctx, "/usr/bin/sudo", append(sudoHome(), args...)...)
}

var runningAsRoot = func() bool { return os.Geteuid() == 0 }
