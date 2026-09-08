package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/processlaunch"
)

type Runner interface {
	Run(context.Context, string, ...string) error
}

type OutputRunner interface {
	Runner
	Output(context.Context, string, ...string) (string, error)
}

type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, name string, arguments ...string) error {
	_, err := (ExecRunner{}).Output(ctx, name, arguments...)
	return err
}

func (ExecRunner) Output(ctx context.Context, name string, arguments ...string) (string, error) {
	output := &boundedCommandOutput{limit: 8 << 10}
	command := exec.CommandContext(ctx, name, arguments...)
	processlaunch.ConfigureBackground(command)
	command.Stdout, command.Stderr = output, output
	if err := command.Run(); err != nil {
		return output.String(), &CommandError{Tool: name, Output: output.String(), Cause: err}
	}
	return output.String(), nil
}

type CommandError struct {
	Tool   string
	Output string
	Cause  error
}

func (e *CommandError) Error() string {
	if e.Output == "" {
		return fmt.Sprintf("%s: %v", e.Tool, e.Cause)
	}
	return fmt.Sprintf("%s: %v: %s", e.Tool, e.Cause, e.Output)
}
func (e *CommandError) Unwrap() error { return e.Cause }

type boundedCommandOutput struct {
	bytes []byte
	limit int
}

func (w *boundedCommandOutput) Write(data []byte) (int, error) {
	consumed := len(data)
	remaining := w.limit - len(w.bytes)
	if remaining > 0 {
		if len(data) > remaining {
			data = data[:remaining]
		}
		w.bytes = append(w.bytes, data...)
	}
	return consumed, nil
}
func (w *boundedCommandOutput) String() string { return string(w.bytes) }

type SystemdController struct {
	Runner Runner
	Unit   string
	User   bool
}

func (c SystemdController) unit() string {
	if c.Unit != "" {
		return c.Unit
	}
	return "paperboat-runtime-host.service"
}

func (c SystemdController) Apply(ctx context.Context, _ string, upgrading bool) error {
	if c.Runner == nil {
		return ErrInvalidDefinition
	}
	args := func(values ...string) []string {
		if c.User {
			return append([]string{"--user"}, values...)
		}
		return values
	}
	if err := c.Runner.Run(ctx, "systemctl", args("daemon-reload")...); err != nil {
		return err
	}
	if err := c.Runner.Run(ctx, "systemctl", args("enable", "--now", c.unit())...); err != nil {
		return err
	}
	if upgrading {
		if err := c.Runner.Run(ctx, "systemctl", args("restart", c.unit())...); err != nil {
			return err
		}
	}
	return c.Runner.Run(ctx, "systemctl", args("is-active", "--quiet", c.unit())...)
}

func (c SystemdController) Remove(ctx context.Context, definitionPath string) error {
	if c.Runner == nil {
		return ErrInvalidDefinition
	}
	args := func(values ...string) []string {
		if c.User {
			return append([]string{"--user"}, values...)
		}
		return values
	}
	if err := c.Runner.Run(ctx, "systemctl", args("disable", "--now", c.unit())...); err != nil && !systemdUnitAbsent(err) {
		return err
	}
	if err := os.Remove(definitionPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := c.Runner.Run(ctx, "systemctl", args("daemon-reload")...); err != nil {
		return err
	}
	if err := c.Runner.Run(ctx, "systemctl", args("reset-failed", c.unit())...); err != nil && !systemdUnitAbsent(err) {
		return err
	}
	return nil
}

func systemdUnitAbsent(err error) bool {
	var commandErr *CommandError
	if !errors.As(err, &commandErr) {
		return false
	}
	output := strings.ToLower(commandErr.Output)
	return strings.Contains(output, "unit") && (strings.Contains(output, "not loaded") || strings.Contains(output, "not found") || strings.Contains(output, "does not exist"))
}

type LaunchdController struct {
	Runner     Runner
	UID        int
	Label      string
	UserDomain bool
}

func launchdServiceAbsent(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "no such process") ||
		strings.Contains(message, "could not find service") ||
		strings.Contains(message, "service not found") ||
		strings.Contains(message, "does not exist")
}

func launchdStartRetryable(err error) bool {
	if launchdServiceAbsent(err) {
		return true
	}
	var commandErr *CommandError
	if !errors.As(err, &commandErr) {
		return false
	}
	var exitError interface{ ExitCode() int }
	return errors.As(commandErr.Cause, &exitError) && exitError.ExitCode() == 37
}

func (c LaunchdController) label() string {
	if c.Label != "" {
		return c.Label
	}
	return Label
}

func (c LaunchdController) domain() string {
	if c.UserDomain {
		return fmt.Sprintf("gui/%d", c.UID)
	}
	return "system"
}

func (c LaunchdController) service() string { return c.domain() + "/" + c.label() }

func (c LaunchdController) Apply(ctx context.Context, path string, upgrading bool) error {
	if c.Runner == nil || c.UID < 0 {
		return ErrInvalidDefinition
	}
	operationCtx, cancel, err := nativeServiceContext(ctx)
	if err != nil {
		return err
	}
	defer cancel()
	domain := "system"
	if c.UserDomain {
		domain = fmt.Sprintf("gui/%d", c.UID)
	}
	service := domain + "/" + c.label()
	if upgrading {
		if err := c.Runner.Run(operationCtx, "launchctl", "bootout", service); err != nil && !launchdServiceAbsent(err) {
			return err
		}
	}
	return c.startJob(operationCtx, path, true, true)
}

// startJob owns launchd's bootout/bootstrap reservation race for both install
// and transactional restart paths. A successful stale print is not sufficient:
// the label must still accept kickstart before the job is considered started.
func (c LaunchdController) startJob(ctx context.Context, path string, bootstrap, verify bool) error {
	service := c.service()
	for {
		if bootstrap {
			if err := c.Runner.Run(ctx, "launchctl", "bootstrap", c.domain(), path); err != nil {
				if c.Runner.Run(ctx, "launchctl", "print", service) != nil {
					if waitErr := waitLaunchdRetry(ctx, err); waitErr != nil {
						return waitErr
					}
					continue
				}
			}
		}
		err := c.Runner.Run(ctx, "launchctl", "kickstart", "-k", service)
		if err == nil {
			if verify {
				return c.Runner.Run(ctx, "launchctl", "print", service)
			}
			return nil
		}
		if !launchdStartRetryable(err) || path == "" {
			return err
		}
		bootstrap = true
		if waitErr := waitLaunchdRetry(ctx, ErrLifecycleNotReady); waitErr != nil {
			return waitErr
		}
	}
}

func waitLaunchdRetry(ctx context.Context, cause error) error {
	timer := time.NewTimer(500 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return errors.Join(cause, ctx.Err())
	case <-timer.C:
		return nil
	}
}

func (c LaunchdController) Remove(ctx context.Context, _ string) error {
	if c.Runner == nil || c.UID < 0 {
		return ErrInvalidDefinition
	}
	domain := "system"
	if c.UserDomain {
		domain = fmt.Sprintf("gui/%d", c.UID)
	}
	err := c.Runner.Run(ctx, "launchctl", "bootout", domain+"/"+c.label())
	if err != nil && launchdServiceAbsent(err) {
		return nil
	}
	return err
}
