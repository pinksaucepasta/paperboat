//go:build darwin || linux

package managedssh

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// A pb killed outright (SIGKILL, closed terminal) must not leave the native tool running: a retry would race it
// and corrupt the file it was writing (found by the cross-platform F5-03 resume check).
func TestOpenSSHExecutorToolDiesWithKilledParent(t *testing.T) {
	binary, _ := os.Executable()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	child := filepath.Join(t.TempDir(), "child")
	command := exec.CommandContext(ctx, binary, "-test.run=TestOpenSSHExecutorHelperProcess")
	command.Env = append(os.Environ(), "PB_EXECUTOR_HELPER=supervise_signal", "PB_EXECUTOR_RESULT="+child+".status", "PB_EXECUTOR_CHILD="+child)
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	wait := make(chan error, 1)
	go func() { wait <- command.Wait() }()
	pid := waitPID(t, child, wait)
	defer syscall.Kill(pid, syscall.SIGKILL)
	if err := command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	<-wait
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
			return
		}
	}
	t.Fatal("the native tool kept running after its pb was killed")
}
