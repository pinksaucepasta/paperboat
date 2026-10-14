package selfhost

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

// binaryLauncher serves the current pbh consumer until its Docker producer is
// installed. Both launchers use the single installation supervisor.
type binaryLauncher struct{ directory string }

func (l binaryLauncher) Validate(_ context.Context, capability, _ string) error {
	binary := filepath.Join(l.directory, "paperboat-"+capability)
	info, err := os.Stat(binary)
	if err != nil {
		return fmt.Errorf("runtime binary unavailable: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		return fmt.Errorf("%s runtime binary unavailable", capability)
	}
	return nil
}

func (l binaryLauncher) Run(ctx context.Context, capability, dir string, output io.Writer) error {
	cmd := exec.CommandContext(ctx, filepath.Join(l.directory, "paperboat-"+capability), "run", "--state-dir", dir)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stdout, cmd.Stderr = output, output
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	cmd.WaitDelay = 6 * time.Second
	err := cmd.Run()
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	return err
}
