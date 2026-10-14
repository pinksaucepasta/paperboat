//go:build darwin || linux

package managedssh

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

var ErrOpenSSHExecution = errors.New("OpenSSH execution request is invalid")

type OpenSSHExecutor struct{}

const nativeSSHCancelGrace = 5 * time.Second

// Execute inherits the native terminal and foreground process group while pb
// remains alive to finish its invocation diagnostics after the native tool exits.
func (OpenSSHExecutor) Execute(ctx context.Context, executable string, arguments, environment []string) error {
	if ctx == nil || !validProcessValues(arguments) || !validEnvironment(environment) {
		return ErrOpenSSHExecution
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	path, err := resolveOpenSSHExecutable(executable)
	if err != nil {
		return NativeLaunchError{Err: errors.Join(ErrOpenSSHExecution, err)}
	}
	command := exec.Command(path, arguments...)
	command.Stdin, command.Stdout, command.Stderr = os.Stdin, os.Stdout, os.Stderr
	if environment != nil {
		command.Env = append([]string(nil), environment...)
	}
	// Do not create a process group or proxy stdio: the shell's foreground group
	// delivers terminal stop/continue to both processes, and OpenSSH owns raw mode.
	signals := make(chan os.Signal, 8)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(signals)
	if err := command.Start(); err != nil {
		return NativeLaunchError{Err: err}
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	cancellation := ctx.Done()
	var timer *time.Timer
	var deadline <-chan time.Time
	forwarded := false
	beginDrain := func() {
		if timer == nil {
			timer = time.NewTimer(nativeSSHCancelGrace)
			deadline = timer.C
		}
	}
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	for {
		select {
		case err := <-done:
			if err == nil {
				return nil
			}
			var exited *exec.ExitError
			if errors.As(err, &exited) {
				status := exited.Sys().(syscall.WaitStatus)
				code := status.ExitStatus()
				if status.Signaled() {
					code = 128 + int(status.Signal())
				}
				return NativeExitError{Code: code, Err: err}
			}
			return NativeLaunchError{Err: err}
		case received := <-signals:
			_ = command.Process.Signal(received)
			forwarded = true
			// Signal-only forwarding preserves the native tool's response. The
			// owner's cancellation context controls bounded termination.
		case <-cancellation:
			cancellation = nil
			// NotifyContext can wake before this observer consumes the same OS signal.
			select {
			case received := <-signals:
				_ = command.Process.Signal(received)
				forwarded = true
			default:
			}
			if !forwarded {
				_ = command.Process.Signal(syscall.SIGTERM)
			}
			beginDrain()
		case <-deadline:
			deadline = nil
			_ = command.Process.Kill()
		}
	}
}

func validProcessValues(values []string) bool {
	if len(values) == 0 || len(values) > 4096 {
		return false
	}
	for _, value := range values {
		if strings.ContainsRune(value, 0) || len(value) > 1<<20 {
			return false
		}
	}
	return true
}

func validEnvironment(values []string) bool {
	if values == nil {
		return true
	}
	if len(values) > 16384 {
		return false
	}
	for _, value := range values {
		name, _, ok := strings.Cut(value, "=")
		if !ok || name == "" || strings.ContainsAny(name, "\x00=") || strings.ContainsRune(value, 0) || len(value) > 1<<20 {
			return false
		}
	}
	return true
}
