//go:build !windows

package hostruntimecmd

import (
	"context"
	"os"
	"os/exec"
	"time"
)

// privilegedPipeWaitDelay bounds Run after its context ends: a killed sudo's root child can keep our pipes open.
// Run then returns exec.ErrWaitDelay also after a clean exit whose child kept a pipe; callers treat that as success.
const privilegedPipeWaitDelay = 5 * time.Second

// sudoCommand starts /usr/bin/sudo with sudoHome's flags first (none on Linux). As root it runs the
// command after "--" directly: root needs no sudo, and minimal container images have none.
func sudoCommand(ctx context.Context, args ...string) *exec.Cmd {
	if runningAsRoot() && len(args) > 1 && args[0] == "--" {
		return exec.CommandContext(ctx, args[1], args[2:]...)
	}
	return exec.CommandContext(ctx, sudoExecutable, append(sudoHome(), args...)...)
}

var runningAsRoot = func() bool { return os.Geteuid() == 0 }

// sudoWithoutPrompt reports whether sudo would run now without asking for a password.
func sudoWithoutPrompt(ctx context.Context) bool {
	if runningAsRoot() {
		return true
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return sudoCommand(ctx, "-n", "true").Run() == nil
}
