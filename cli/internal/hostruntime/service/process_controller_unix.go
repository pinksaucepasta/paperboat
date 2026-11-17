//go:build !windows

package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// processStopLimit bounds a stop: SIGTERM first, SIGKILL when the child ignores it, then a short wait for the
// kernel to reap it.
const (
	processStopLimit = 30 * time.Second
	processKillLimit = 5 * time.Second
)

// ErrProcessShutdown is returned by a start after Shutdown: the supervisor is leaving and no child may outlive it.
var ErrProcessShutdown = errors.New("the service supervisor is shutting down")

// ProcessController runs one service as a child of the calling process, for a host with no service manager (a
// container). It is the NativeLifecycleController of that child: the declaration file the Installer writes stays
// inert, Apply and Start are the only places a process begins, and a child that exits is simply not running, so the
// next start request begins a new one, as a Restart= policy would.
type ProcessController struct {
	// Command returns a new command for each start; the controller owns its Start, Wait and signals.
	Command func() *exec.Cmd

	mu     sync.Mutex
	cmd    *exec.Cmd
	done   chan struct{}
	closed bool
}

var _ NativeLifecycleController = (*ProcessController)(nil)

func (c *ProcessController) running() bool {
	if c.done == nil {
		return false
	}
	select {
	case <-c.done:
		return false
	default:
		return true
	}
}

func (c *ProcessController) start() error {
	if c.closed {
		return ErrProcessShutdown
	}
	if c.Command == nil {
		return ErrInvalidDefinition
	}
	command := c.Command()
	if command == nil {
		return ErrInvalidDefinition
	}
	if err := command.Start(); err != nil {
		return err
	}
	done := make(chan struct{})
	c.cmd, c.done = command, done
	go func() {
		_ = command.Wait()
		close(done)
	}()
	return nil
}

func (c *ProcessController) stop(ctx context.Context) error {
	if !c.running() {
		return nil
	}
	_ = c.cmd.Process.Signal(syscall.SIGTERM)
	timer := time.NewTimer(processStopLimit)
	defer timer.Stop()
	select {
	case <-c.done:
		return nil
	case <-timer.C:
	case <-ctx.Done():
	}
	_ = c.cmd.Process.Kill()
	select {
	case <-c.done:
		return nil
	case <-time.After(processKillLimit):
		return fmt.Errorf("service process %d did not exit after SIGKILL", c.cmd.Process.Pid)
	}
}

// Shutdown stops the child and refuses every later start, so a request still in flight when the supervisor
// leaves cannot begin a child that outlives it.
func (c *ProcessController) Shutdown(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	return c.stop(ctx)
}

func (c *ProcessController) Apply(ctx context.Context, _ string, upgrading bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if upgrading && c.running() {
		if err := c.stop(ctx); err != nil {
			return err
		}
	}
	if c.running() {
		return nil
	}
	return c.start()
}

func (c *ProcessController) Remove(ctx context.Context, definitionPath string) error {
	c.mu.Lock()
	err := c.stop(ctx)
	c.mu.Unlock()
	if removeErr := os.Remove(definitionPath); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
		err = errors.Join(err, removeErr)
	}
	return err
}

func (c *ProcessController) Inspect(context.Context, string) (NativeControllerStatus, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	running := c.running()
	return NativeControllerStatus{Registered: running, Enabled: running, Running: running, Ready: running}, nil
}

func (c *ProcessController) Enable(context.Context, string) error  { return nil }
func (c *ProcessController) Disable(context.Context, string) error { return nil }

func (c *ProcessController) Start(context.Context, string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.running() {
		return nil
	}
	return c.start()
}

func (c *ProcessController) Stop(ctx context.Context, _ string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stop(ctx)
}
