//go:build !windows

package hostruntimecmd

import (
	"context"
	"os/exec"
)

// sudoCommand starts /usr/bin/sudo with sudoHome's flags first (none on Linux).
func sudoCommand(ctx context.Context, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, "/usr/bin/sudo", append(sudoHome(), args...)...)
}
