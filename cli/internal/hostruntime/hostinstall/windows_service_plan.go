package hostinstall

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
	"github.com/pinksaucepasta/paperboat/internal/windowsopenssh"
)

// skipUnobtainableWindowsSSH lets an installation, update or repair finish on a machine that cannot supply
// OpenSSH (no App Installer on Windows Server, LTSC or Sandbox), as Linux and macOS do without sshd: the
// worker reports managed SSH as not ready and the next install, update or repair prepares it again. Trust,
// ownership and cancellation failures still abort.
func skipUnobtainableWindowsSSH(ctx context.Context, err error) error {
	if err != nil && ctx.Err() == nil && windowsopenssh.Unobtainable(err) {
		return nil
	}
	return err
}

// windowsSSHPreparationBudget bounds the OpenSSH step of an install, pair or repair: obtaining the package may fall
// back to the PowerShell Gallery (5 min) and Install-WinGetPackage (10 min), which exceed the 3 min the elevated
// bridge allows the whole operation (winpool-3, 2026-11-10: three minutes of silence, then a rollback).
var windowsSSHPreparationBudget = 75 * time.Second

// windowsSSHTimeoutNotice tells the user, once per expiry, that managed SSH was skipped and how to get it later.
var windowsSSHTimeoutNotice = func(err error) {
	fmt.Fprintf(os.Stderr, "Managed SSH was not prepared within %s (%v); installation continues without it. Run `pb doctor --repair` to prepare it.\n", windowsSSHPreparationBudget, err)
}

// prepareWindowsSSHWithin runs one OpenSSH preparation step under its own budget. When the budget expires the machine
// could not supply OpenSSH in time: that is announced (windowsSSHTimeoutNotice) and the install continues without managed
// SSH like any other unobtainable case. Only the expiry itself or an unobtainable package is skipped: a trust or ownership
// refusal that arrives after the budget still aborts, as does an expiry of the caller's own context.
func prepareWindowsSSHWithin(ctx context.Context, step func(context.Context) error) error {
	return prepareWindowsSSHWithinAttempts(ctx, 1, step)
}

// prepareWindowsSSHWithinAttempts is prepareWindowsSSHWithin with up to attempts budgets: an attempt whose budget
// expires is tried again, and only the last expiry is skipped.
func prepareWindowsSSHWithinAttempts(ctx context.Context, attempts int, step func(context.Context) error) error {
	for attempt := 1; ; attempt++ {
		bounded, cancel := context.WithTimeout(ctx, windowsSSHPreparationBudget)
		err := step(bounded)
		expired := err != nil && ctx.Err() == nil && bounded.Err() != nil
		cancel()
		if expired && attempt < attempts {
			continue
		}
		if expired && (errors.Is(err, context.DeadlineExceeded) || windowsopenssh.Unobtainable(err)) {
			windowsSSHTimeoutNotice(err)
			return nil
		}
		return skipUnobtainableWindowsSSH(ctx, err)
	}
}

var standaloneVersionPattern = regexp.MustCompile(`^[0-9]{4}\.[0-9]{2}\.[0-9]{2}\.[0-9]+$`)

type windowsRuntimeServiceDefinition struct {
	kind, executable string
	arguments        []string
}

// windowsServicePlanStep is the small lifecycle surface needed by the
// ordered Windows role plan. Keeping the sequencing/rollback policy separate
// from the native service implementation makes it deterministic to test and
// prevents a later role failure from leaving a service created by this exact
// attempt behind.
type windowsServicePlanStep interface {
	DefinitionPath() string
	Install(context.Context) error
	Start(context.Context) error
	Uninstall(context.Context) error
}

const windowsServicePlanRollbackTimeout = 45 * time.Second

func windowsServiceDeclarationPresent(path string) (bool, error) {
	_, err := os.Lstat(path)
	if installNotExistOnly(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// executeWindowsServiceSteps applies one ordered role plan. The
// preexisting callback must report the native service-manager registration
// observed before the declaration is written. This is deliberately separate
// from declaration-file existence: a stale declaration with no SCM service is
// still created by this attempt and must be removed if a later step fails.
// The returned cleanup removes only registrations that did not exist before
// this call. On an install/start error, the same cleanup is run before the
// error is returned. Existing registrations are deliberately never
// uninstalled by a failed replacement.
func executeWindowsServiceSteps(
	ctx context.Context,
	definitions []windowsRuntimeServiceDefinition,
	makeStep func(windowsRuntimeServiceDefinition) (windowsServicePlanStep, error),
	preexisting func(context.Context, string) (bool, error),
	startAfterInstall bool,
) (func() error, error) {
	return executeWindowsServiceStepsWithHook(ctx, definitions, makeStep, preexisting, startAfterInstall, nil)
}

func executeWindowsServiceStepsWithHook(
	ctx context.Context,
	definitions []windowsRuntimeServiceDefinition,
	makeStep func(windowsRuntimeServiceDefinition) (windowsServicePlanStep, error),
	preexisting func(context.Context, string) (bool, error),
	startAfterInstall bool,
	afterStart func(int, windowsRuntimeServiceDefinition) error,
) (func() error, error) {
	if ctx == nil || makeStep == nil {
		return nil, service.ErrInvalidDefinition
	}
	if preexisting == nil {
		preexisting = func(_ context.Context, path string) (bool, error) {
			return windowsServiceDeclarationPresent(path)
		}
	}
	created := make([]windowsServicePlanStep, 0, len(definitions))
	rolledBack := false
	var rollbackErr error
	rollback := func() error {
		if rolledBack {
			return rollbackErr
		}
		rolledBack = true
		rollbackContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), windowsServicePlanRollbackTimeout)
		defer cancel()
		var result error
		for index := len(created) - 1; index >= 0; index-- {
			if err := created[index].Uninstall(rollbackContext); err != nil {
				result = errors.Join(result, err)
			}
		}
		rollbackErr = hostInstallFailure("Windows service rollback failed", result)
		return rollbackErr
	}
	for index, definition := range definitions {
		step, err := makeStep(definition)
		if err != nil {
			return nil, errors.Join(err, rollback())
		}
		if step == nil || step.DefinitionPath() == "" {
			return nil, errors.Join(service.ErrInvalidDefinition, rollback())
		}
		present, err := preexisting(ctx, step.DefinitionPath())
		if err != nil {
			return nil, errors.Join(err, rollback())
		}
		// Record ownership before Install. Native Enable can create an SCM
		// entry and then return an error while applying recovery/configuration;
		// waiting until Install succeeds would strand that partial registration.
		if !present {
			created = append(created, step)
		}
		if err := step.Install(ctx); err != nil {
			return nil, errors.Join(hostInstallFailure("install Windows "+windowsServiceRole(definition.kind)+" service", err), rollback())
		}
		if startAfterInstall {
			if err := step.Start(ctx); err != nil {
				return nil, errors.Join(hostInstallFailure("start Windows "+windowsServiceRole(definition.kind)+" service", err), rollback())
			}
			if afterStart != nil {
				if err := afterStart(index, definition); err != nil {
					return nil, errors.Join(hostInstallFailure("verify Windows "+windowsServiceRole(definition.kind)+" service readiness", err), rollback())
				}
			}
		}
	}
	return rollback, nil
}

func windowsRuntimeServiceDefinitions(layout service.Layout, pinned ...string) []windowsRuntimeServiceDefinition {
	executable := layout.Binary
	if len(pinned) == 1 {
		executable = pinned[0]
	}
	var instanceArgs []string
	if layout.Instance != "" {
		instanceArgs = []string{"--instance", layout.Instance}
	}
	return []windowsRuntimeServiceDefinition{
		{kind: service.HostdKind, executable: executable, arguments: append([]string{"daemon", "__runtime-hostd"}, instanceArgs...)},
		{kind: service.DaemonKind, executable: executable, arguments: append([]string{"daemon", "__runtime-local-daemon"}, instanceArgs...)},
		{kind: service.UpdaterKind, executable: executable, arguments: append([]string{"daemon", "__runtime-updated"}, instanceArgs...)},
	}
}

// executeWindowsServiceInstallPlan is the ordering boundary for a Windows
// runtime installation. Hostd performs a managed-SSH loopback reconciliation during
// startup, so machine recovery must not restore/start Hostd until Paperboat's
// dedicated OpenSSH service is installed and healthy. Keeping recovery in this
// plan also covers a crash journal whose rollback would otherwise start Hostd
// before the SSH callback runs.
func executeWindowsServiceInstallPlan(installSSH, recoverRuntime, installRuntime, cleanupSSH func() error) error {
	if installSSH == nil || recoverRuntime == nil || installRuntime == nil || cleanupSSH == nil {
		return ErrInvalidRequest
	}
	// The worker reconciles the managed SSH loopback endpoint as it starts (and
	// keeps retrying without it). The service SID used by the host-key ACL is
	// deterministic, so OpenSSH can be installed before Hostd is first registered.
	if err := installSSH(); err != nil {
		return err
	}
	if err := recoverRuntime(); err != nil {
		return errors.Join(err, cleanupSSH())
	}
	if err := installRuntime(); err != nil {
		return errors.Join(err, cleanupSSH())
	}
	return nil
}

// executeWindowsServiceRepairPlan is the bounded repair ordering boundary.
// Machine repair keeps the managed-SSH service installed from the first
// phase through lifecycle repair. Removing it between recovery and repair
// creates a Windows SCM deletion race and lets Hostd start without its
// required loopback authority. Cleanup is reserved for a failed phase after
// the SSH prerequisite has been established.
func executeWindowsServiceRepairPlan(installSSH, recoverRuntime, repairRuntime, repairServices, cleanupSSH func() error) error {
	if installSSH == nil || recoverRuntime == nil || repairRuntime == nil || repairServices == nil || cleanupSSH == nil {
		return ErrInvalidRequest
	}
	if err := installSSH(); err != nil {
		return err
	}
	if err := recoverRuntime(); err != nil {
		return errors.Join(err, cleanupSSH())
	}
	if err := repairRuntime(); err != nil {
		return errors.Join(err, cleanupSSH())
	}
	if err := repairServices(); err != nil {
		return errors.Join(err, cleanupSSH())
	}
	return nil
}

func windowsActivatorExecutableOwned(layout service.Layout, executable string) bool {
	prefix := strings.TrimRight(layout.ReleasesRoot, `\`) + `\versions\`
	if len(executable) <= len(prefix) || !strings.EqualFold(executable[:len(prefix)], prefix) {
		return false
	}
	parts := strings.Split(executable[len(prefix):], `\`)
	return len(parts) == 2 && standaloneVersionPattern.MatchString(parts[0]) && strings.EqualFold(parts[1], "pb.exe")
}

func windowsActivatorServiceOwned(layout service.Layout, executable string, arguments []string, account string) bool {
	return layout.Instance != "" && windowsActivatorExecutableOwned(layout, executable) && len(arguments) == 4 && arguments[0] == "daemon" && arguments[1] == "__runtime-activate" && arguments[2] == "--instance" && arguments[3] == layout.Instance && strings.EqualFold(strings.TrimSpace(account), "LocalSystem")
}

func windowsServiceRole(kind string) string {
	switch kind {
	case service.HostdKind:
		return "hostd"
	case service.DaemonKind:
		return "daemon"
	case service.UpdaterKind:
		return "updater"
	default:
		return "runtime"
	}
}
