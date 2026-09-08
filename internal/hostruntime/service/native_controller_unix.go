//go:build !windows

package service

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"
)

func (c SystemdController) Inspect(ctx context.Context, _ string) (NativeControllerStatus, error) {
	runner, ok := c.Runner.(OutputRunner)
	if !ok {
		return NativeControllerStatus{}, ErrLifecycleInvalid
	}
	operationCtx, cancel, err := nativeServiceContext(ctx)
	if err != nil {
		return NativeControllerStatus{}, err
	}
	defer cancel()
	arguments := []string{"show", c.unit(), "--property=LoadState", "--property=UnitFileState", "--property=ActiveState", "--property=SubState"}
	if c.User {
		arguments = append([]string{"--user"}, arguments...)
	}
	output, err := runner.Output(operationCtx, "systemctl", arguments...)
	if err != nil {
		if systemdUnitAbsent(err) {
			return NativeControllerStatus{}, nil
		}
		return NativeControllerStatus{}, err
	}
	properties := parseNativeProperties(output)
	if properties["LoadState"] == "not-found" {
		return NativeControllerStatus{}, nil
	}
	registered := properties["LoadState"] == "loaded"
	enabled := properties["UnitFileState"] == "enabled"
	running := properties["ActiveState"] == "active"
	ready := running && (properties["SubState"] == "running" || properties["SubState"] == "exited")
	return NativeControllerStatus{Registered: registered, Enabled: enabled, Running: running, Ready: ready}, nil
}

func (c SystemdController) Enable(ctx context.Context, _ string) error {
	if c.Runner == nil {
		return ErrLifecycleInvalid
	}
	operationCtx, cancel, err := nativeServiceContext(ctx)
	if err != nil {
		return err
	}
	defer cancel()
	arguments := func(values ...string) []string {
		if c.User {
			return append([]string{"--user"}, values...)
		}
		return values
	}
	if err := c.Runner.Run(operationCtx, "systemctl", arguments("daemon-reload")...); err != nil {
		return err
	}
	return c.Runner.Run(operationCtx, "systemctl", arguments("enable", c.unit())...)
}

func (c SystemdController) Disable(ctx context.Context, _ string) error {
	if c.Runner == nil {
		return ErrLifecycleInvalid
	}
	operationCtx, cancel, err := nativeServiceContext(ctx)
	if err != nil {
		return err
	}
	defer cancel()
	arguments := []string{"disable", c.unit()}
	if c.User {
		arguments = append([]string{"--user"}, arguments...)
	}
	err = c.Runner.Run(operationCtx, "systemctl", arguments...)
	if systemdUnitAbsent(err) {
		return nil
	}
	return err
}

func (c SystemdController) Start(ctx context.Context, _ string) error {
	if c.Runner == nil {
		return ErrLifecycleInvalid
	}
	operationCtx, cancel, err := nativeServiceContext(ctx)
	if err != nil {
		return err
	}
	defer cancel()
	arguments := func(values ...string) []string {
		if c.User {
			return append([]string{"--user"}, values...)
		}
		return values
	}
	if err := c.Runner.Run(operationCtx, "systemctl", arguments("start", c.unit())...); err != nil {
		return err
	}
	return c.Runner.Run(operationCtx, "systemctl", arguments("is-active", "--quiet", c.unit())...)
}

func (c SystemdController) Stop(ctx context.Context, _ string) error {
	if c.Runner == nil {
		return ErrLifecycleInvalid
	}
	operationCtx, cancel, err := nativeServiceContext(ctx)
	if err != nil {
		return err
	}
	defer cancel()
	arguments := []string{"stop", c.unit()}
	if c.User {
		arguments = append([]string{"--user"}, arguments...)
	}
	err = c.Runner.Run(operationCtx, "systemctl", arguments...)
	if systemdUnitAbsent(err) {
		return nil
	}
	return err
}

func (c LaunchdController) Inspect(ctx context.Context, _ string) (NativeControllerStatus, error) {
	runner, ok := c.Runner.(OutputRunner)
	if !ok || c.UID < 0 {
		return NativeControllerStatus{}, ErrLifecycleInvalid
	}
	operationCtx, cancel, err := nativeServiceContext(ctx)
	if err != nil {
		return NativeControllerStatus{}, err
	}
	defer cancel()
	output, err := runner.Output(operationCtx, "launchctl", "print", c.service())
	if err != nil {
		if launchdServiceAbsent(err) {
			return NativeControllerStatus{}, nil
		}
		return NativeControllerStatus{}, err
	}
	status := parseLaunchdPrintStatus(output)
	// launchctl does not always emit a top-level `state = running` line for a
	// healthy KeepAlive LaunchDaemon. A positive active count together with a
	// process id is the stable native running signal in that projection. A
	// spawn-scheduled job is failed, even if a stale or nested process
	// projection happens to contain a positive count and pid.
	running := status.state == "running" || status.activeCount > 0 && status.pid > 0
	if status.state == "spawn scheduled" {
		running = false
	}
	return NativeControllerStatus{Registered: true, Enabled: true, Running: running, Ready: running}, nil
}

func (c LaunchdController) Enable(ctx context.Context, path string) error {
	if c.Runner == nil || c.UID < 0 {
		return ErrLifecycleInvalid
	}
	operationCtx, cancel, err := nativeServiceContext(ctx)
	if err != nil {
		return err
	}
	defer cancel()
	err = c.Runner.Run(operationCtx, "launchctl", "bootstrap", c.domain(), path)
	if err == nil {
		return nil
	}
	if probeErr := c.Runner.Run(operationCtx, "launchctl", "print", c.service()); probeErr == nil {
		return nil
	}
	return err
}

func (c LaunchdController) Disable(ctx context.Context, _ string) error {
	if c.Runner == nil || c.UID < 0 {
		return ErrLifecycleInvalid
	}
	operationCtx, cancel, err := nativeServiceContext(ctx)
	if err != nil {
		return err
	}
	defer cancel()
	err = c.Runner.Run(operationCtx, "launchctl", "bootout", c.service())
	if launchdServiceAbsent(err) {
		return nil
	}
	return err
}

func (c LaunchdController) Start(ctx context.Context, path string) error {
	runner, ok := c.Runner.(OutputRunner)
	if !ok || c.UID < 0 {
		return ErrLifecycleInvalid
	}
	operationCtx, cancel, err := nativeServiceContext(ctx)
	if err != nil {
		return err
	}
	defer cancel()
	if err := c.startJob(operationCtx, path, false, false); err != nil {
		return err
	}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		output, printErr := runner.Output(operationCtx, "launchctl", "print", c.service())
		if printErr != nil {
			return printErr
		}
		status := parseLaunchdPrintStatus(output)
		if status.state == "running" || status.state != "spawn scheduled" && status.activeCount > 0 && status.pid > 0 {
			return nil
		}
		select {
		case <-operationCtx.Done():
			return errors.Join(ErrLifecycleNotReady, operationCtx.Err())
		case <-ticker.C:
		}
	}
}

func (c LaunchdController) Stop(ctx context.Context, _ string) error {
	if c.Runner == nil || c.UID < 0 {
		return ErrLifecycleInvalid
	}
	operationCtx, cancel, err := nativeServiceContext(ctx)
	if err != nil {
		return err
	}
	defer cancel()
	// launchd KeepAlive/RunAtLoad would restart a merely killed service. Boot
	// the job out to make Stop durable, while leaving the declaration plist in
	// place so Start/Repair can bootstrap the exact same bytes again.
	err = c.Runner.Run(operationCtx, "launchctl", "bootout", c.service())
	if launchdServiceAbsent(err) {
		return nil
	}
	return err
}

type launchdPrintStatus struct {
	state       string
	activeCount int
	pid         int
}

func parseLaunchdPrintStatus(output string) launchdPrintStatus {
	var status launchdPrintStatus
	depth := 0
	rootStarted := false
	for _, rawLine := range strings.Split(output, "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" {
			continue
		}

		if !rootStarted {
			if strings.Contains(line, "{") {
				rootStarted = true
				depth = launchdBraceDelta(line)
				continue
			}
			// Keep accepting the compact key/value projection used by older
			// launchctl versions and tests that omit the enclosing block.
			parseLaunchdPrintField(line, &status)
			continue
		}

		if depth == 1 {
			parseLaunchdPrintField(line, &status)
		}
		depth += launchdBraceDelta(line)
		if depth <= 0 {
			break
		}
	}
	return status
}

func parseLaunchdPrintField(line string, status *launchdPrintStatus) {
	key, value, ok := strings.Cut(line, "=")
	if !ok {
		return
	}
	value = strings.TrimSpace(value)
	switch strings.TrimSpace(key) {
	case "state":
		status.state = value
	case "active count":
		if activeCount, err := strconv.Atoi(value); err == nil {
			status.activeCount = activeCount
		}
	case "pid":
		if pid, err := strconv.Atoi(value); err == nil {
			status.pid = pid
		}
	}
}

func launchdBraceDelta(line string) int {
	delta := 0
	inQuotes := false
	escaped := false
	for _, character := range line {
		if escaped {
			escaped = false
			continue
		}
		if inQuotes {
			switch character {
			case '\\':
				escaped = true
			case '"':
				inQuotes = false
			}
			continue
		}
		switch character {
		case '"':
			inQuotes = true
		case '{':
			delta++
		case '}':
			delta--
		}
	}
	return delta
}

func parseNativeProperties(output string) map[string]string {
	properties := make(map[string]string)
	for _, line := range strings.Split(output, "\n") {
		key, value, ok := strings.Cut(line, "=")
		if ok {
			properties[strings.TrimSpace(key)] = strings.TrimSpace(value)
		}
	}
	return properties
}

var (
	_ NativeLifecycleController = SystemdController{}
	_ NativeLifecycleController = LaunchdController{}
)
