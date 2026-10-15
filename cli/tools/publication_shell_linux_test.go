package tools

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

// Route the real filesystem/installer contract owners through make gate.
// Publication uses Linux renameat2; these fixtures never touch a live origin.
func TestLaunchPublicationShellContracts(t *testing.T) {
	for _, script := range []string{"test-publish-tuf-origin.sh", "test-macos-install-preservation.sh"} {
		t.Run(script, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			command := exec.CommandContext(ctx, "bash", script)
			command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			command.Cancel = func() error {
				err := syscall.Kill(-command.Process.Pid, syscall.SIGTERM)
				if errors.Is(err, syscall.ESRCH) {
					return os.ErrProcessDone
				}
				return err
			}
			command.WaitDelay = 5 * time.Second
			if output, err := command.CombinedOutput(); err != nil {
				t.Fatalf("%s: %v: %s", script, err, output)
			}
		})
	}
}
