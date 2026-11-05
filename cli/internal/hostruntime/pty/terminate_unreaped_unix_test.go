//go:build darwin || linux

package pty

import (
	"context"
	"os/exec"
	"testing"
	"time"

	creackpty "github.com/creack/pty"
)

// Darwin refuses a signal to a process group whose leader has exited but has not been reaped (EPERM;
// ESRCH once reaped, macOS run 42033323127), the window TestIdentificationConcurrentCloseReturnsUnavailable
// hit. Terminate in that window waits for the exit instead of returning the refusal.
func TestTerminateWaitsForALeaderThatExitedBeforeItWasReaped(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", "exit 0")
	terminal, err := creackpty.Start(cmd)
	if err != nil {
		t.Fatal(err)
	}
	// The master reads an error once the session leader has exited; it stays unreaped until wait runs.
	buffer := make([]byte, 64)
	for {
		if _, err := terminal.Read(buffer); err != nil {
			break
		}
	}
	process := &Process{file: terminal, cmd: cmd, done: make(chan struct{})}
	reap := make(chan struct{})
	go func() { <-reap; process.wait() }()
	// Let Terminate signal before the reap. If the reaper wins anyway the signal finds no process (ESRCH),
	// which Terminate already accepts, so this ordering can only weaken the check, never fail it.
	time.AfterFunc(100*time.Millisecond, func() { close(reap) })
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if _, err := process.Terminate(ctx, 2*time.Second); err != nil {
		t.Fatalf("terminate an exited, unreaped shell: %v", err)
	}
}
