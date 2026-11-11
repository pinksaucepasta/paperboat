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

type ExecRunner struct {
	Environment []string
}

func (runner ExecRunner) Run(ctx context.Context, name string, arguments ...string) error {
	_, err := runner.Output(ctx, name, arguments...)
	return err
}

func (runner ExecRunner) Output(ctx context.Context, name string, arguments ...string) (string, error) {
	output := &boundedCommandOutput{limit: 8 << 10}
	command := exec.CommandContext(ctx, name, arguments...)
	command.Env = runner.Environment
	if command.Env == nil {
		command.Env = withoutServiceManagerProtocol(os.Environ())
	}
	processlaunch.ConfigureBackground(command)
	command.Stdout, command.Stderr = output, output
	if err := command.Run(); err != nil {
		return output.String(), &CommandError{Tool: name, Output: output.String(), Cause: err}
	}
	return output.String(), nil
}

// withoutServiceManagerProtocol drops the variables a service manager sets for the service's own main
// process. A child that inherits them (systemctl, for one) sends notifications that the manager
// rejects as coming from a process other than the main one, logging it every time.
func withoutServiceManagerProtocol(environment []string) []string {
	kept := make([]string, 0, len(environment))
	for _, entry := range environment {
		name, _, _ := strings.Cut(entry, "=")
		switch name {
		case "NOTIFY_SOCKET", "WATCHDOG_PID", "WATCHDOG_USEC", "LISTEN_PID", "LISTEN_FDS", "LISTEN_FDNAMES":
			continue
		}
		kept = append(kept, entry)
	}
	return kept
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

// systemdUserManagerAbsent recognises systemctl --user on a host where the user's systemd instance is
// not running (no login session and no lingering): the user's services cannot be running either.
func systemdUserManagerAbsent(err error) bool {
	var commandErr *CommandError
	if !errors.As(err, &commandErr) {
		return false
	}
	output := strings.ToLower(commandErr.Output)
	return strings.Contains(output, "failed to connect to bus") || strings.Contains(output, "failed to connect to user scope bus")
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
		strings.Contains(message, "domain does not support specified action") ||
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

// launchdDomainUnsupported is launchctl's exit code for a domain that does not exist, such as gui/<uid>
// while that user has no session at the Mac's screen.
const launchdDomainUnsupported = 125

// launchctlExit runs launchctl and returns its exit code (-1 when it cannot run); a variable so tests
// can describe a Mac.
var launchctlExit = func(args ...string) int {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := exec.CommandContext(ctx, "launchctl", args...).Run()
	var exitError *exec.ExitError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &exitError):
		return exitError.ExitCode()
	}
	return -1
}

// domain: a per-user agent lives in gui/<uid> while the user is logged in at the Mac's screen. A user
// who only reaches the Mac over SSH has no gui/<uid> domain; launchd still runs a Background-session
// agent in user/<uid> (launchctl print/bootstrap verified on macOS 26, Nov 2026). A service that was
// started there stays there after a later console login. Tailscale sidesteps the split by running
// tailscaled only as a system LaunchDaemon (ref/tailscale/cmd/tailscaled/install_darwin.go).
func (c LaunchdController) domain() string {
	if !c.UserDomain {
		return "system"
	}
	gui, user := fmt.Sprintf("gui/%d", c.UID), fmt.Sprintf("user/%d", c.UID)
	if launchctlExit("print", user+"/"+c.label()) == 0 || launchctlExit("print", gui) == launchdDomainUnsupported {
		return user
	}
	return gui
}

// userAgentsBackground is true when this controller's agent must declare the Background session type
// because it runs in user/<uid>.
func (c LaunchdController) userAgentsBackground() bool {
	return c.UserDomain && strings.HasPrefix(c.domain(), "user/")
}

func (c LaunchdController) service() string { return c.domain() + "/" + c.label() }

// UserAgentTarget is the launchctl target of a user's agent: gui/<uid>/<label> or user/<uid>/<label>.
func UserAgentTarget(uid int, label string) string {
	return LaunchdController{UID: uid, Label: label, UserDomain: true}.service()
}

func (c LaunchdController) Apply(ctx context.Context, path string, upgrading bool) error {
	if c.Runner == nil || c.UID < 0 {
		return ErrInvalidDefinition
	}
	operationCtx, cancel, err := nativeServiceContext(ctx)
	if err != nil {
		return err
	}
	defer cancel()
	// The old job must be gone before bootstrap: while it is still exiting, bootstrap fails, print
	// and a plain kickstart succeed on the dying job, and the new declaration is never loaded.
	if upgrading {
		if err := c.bootoutAndWait(operationCtx); err != nil {
			return err
		}
	} else if err := c.bootoutOtherDomain(operationCtx); err != nil {
		return err
	}
	return c.startJob(operationCtx, path, true, true)
}

// bootoutOtherDomain removes the job from the per-user domain this controller does not use: an agent bootstrapped
// where a console login later made the other domain current would otherwise run twice, one copy unreachable by
// start and restart. The chosen domain's own job is left alone (booting it out would cut its open streams).
func (c LaunchdController) bootoutOtherDomain(ctx context.Context) error {
	if !c.UserDomain {
		return nil
	}
	chosen := c.domain() + "/"
	for _, service := range c.servicesInAnyDomain() {
		if !strings.HasPrefix(service, chosen) {
			if err := c.bootoutService(ctx, service); err != nil {
				return err
			}
		}
	}
	return nil
}

// startJob owns launchd's bootout/bootstrap reservation race for both install
// and transactional restart paths. A successful stale print is not sufficient:
// the label must still accept kickstart before the job is considered started.
// Callers that need a new process stop the job first (bootout).
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
		// Plain kickstart starts a stopped job and leaves a running one alone, like systemctl start.
		// A RunAtLoad job is already starting after bootstrap; -k killed it and paid a second
		// startup on every macOS install, setup and restart (tailscale: load, then start).
		err := c.Runner.Run(ctx, "launchctl", "kickstart", service)
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
	return c.bootoutAndWait(ctx)
}

// bootout returns before launchd has necessarily reaped the process. Keep the
// declaration until the exact job is absent; SIGTERMed is still registered.
func (c LaunchdController) bootoutAndWait(ctx context.Context) error {
	if c.Runner == nil || c.UID < 0 {
		return ErrInvalidDefinition
	}
	operationCtx, cancel, err := nativeServiceContext(ctx)
	if err != nil {
		return err
	}
	defer cancel()
	for _, service := range c.servicesInAnyDomain() {
		if err = c.bootoutService(operationCtx, service); err != nil {
			return err
		}
	}
	return nil
}

// servicesInAnyDomain: removal must reach the service wherever it was started, whichever session the
// user has now.
func (c LaunchdController) servicesInAnyDomain() []string {
	if !c.UserDomain {
		return []string{c.service()}
	}
	return []string{fmt.Sprintf("gui/%d/%s", c.UID, c.label()), fmt.Sprintf("user/%d/%s", c.UID, c.label())}
}

func (c LaunchdController) bootoutService(operationCtx context.Context, service string) error {
	if err := c.Runner.Run(operationCtx, "launchctl", "bootout", service); err != nil {
		if launchdServiceAbsent(err) {
			return nil
		}
		return err
	}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		err := c.Runner.Run(operationCtx, "launchctl", "print", service)
		if launchdServiceAbsent(err) {
			return nil
		}
		if err != nil {
			return err
		}
		select {
		case <-operationCtx.Done():
			return operationCtx.Err()
		case <-ticker.C:
		}
	}
}
