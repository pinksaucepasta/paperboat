//go:build windows

package localdaemon

import (
	"context"
	"errors"

	"github.com/pinksaucepasta/paperboat/internal/windows/elevation"
	"golang.org/x/sys/windows"
)

// runElevatedDaemonService runs one daemon service action as administrator through the
// elevation bridge: one UAC prompt, none in a terminal that is already elevated.
var runElevatedDaemonService = func(ctx context.Context, executable, action string) error {
	return elevation.RunRuntimeService(ctx, executable, action, nil)
}

// CreateCurrentUserService is pb service install: it starts the account's SCM service and
// registers it first when it is missing. Only this command registers it; a command that
// merely finds no daemon (InstallCurrentUserService) never asks for administrator rights.
func CreateCurrentUserService(ctx context.Context, executable, configPath, serverURL string) error {
	err := installWindowsCurrentUserService(ctx, executable, configPath, serverURL)
	// Registering an SCM service needs administrator rights, and so does opening the
	// service manager from a terminal that is not elevated.
	if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) || errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		return runElevatedDaemonService(ctx, executable, elevation.ActionDaemonInstall)
	}
	return err
}

// UninstallCurrentUserService is pb service uninstall: it stops and removes only the
// account's local daemon service. Removing one that is already gone succeeds.
func UninstallCurrentUserService(ctx context.Context, executable string) error {
	if ctx == nil || !validWindowsExecutable(executable) {
		return ErrInvalidInventoryConfig
	}
	return runElevatedDaemonService(ctx, executable, elevation.ActionDaemonRemove)
}

func InspectCurrentUserService(context.Context, string) (ServiceState, error) {
	owner, err := currentWindowsServiceSID()
	if err != nil {
		return ServiceState{}, err
	}
	installed, err := windowsOwnerServiceInstalled(owner)
	if err != nil || !installed {
		return ServiceState{Installed: installed}, err
	}
	running, err := WindowsLocalDaemonServiceRunning(owner)
	return ServiceState{Installed: true, Running: running}, err
}

func StartCurrentUserService(ctx context.Context, _ string) error {
	owner, err := currentWindowsServiceSID()
	if err != nil {
		return err
	}
	installed, err := windowsOwnerServiceInstalled(owner)
	if err != nil {
		return err
	}
	if !installed {
		return ErrServiceNotInstalled
	}
	return invokeWindowsOwnerServiceStart(ctx, owner)
}

func StopCurrentUserService(ctx context.Context, _ string) error {
	owner, err := CurrentUserSID()
	if err != nil {
		return err
	}
	paths, err := CurrentUserPaths()
	if err != nil {
		return err
	}
	return StopWindowsOwnerService(ctx, paths.LockPath, owner)
}
