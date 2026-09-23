//go:build windows

package localdaemon

import (
	"context"
	"errors"
)

func UninstallCurrentUserService(context.Context, string) error {
	return errors.New("the Windows local daemon is owned by the managed Paperboat installation; use the Paperboat uninstaller to remove it")
}

func InspectCurrentUserService(context.Context, string) (ServiceState, error) {
	owner, err := CurrentUserSID()
	if err != nil {
		return ServiceState{}, err
	}
	installed, err := WindowsLocalDaemonServiceInstalled(owner)
	if err != nil || !installed {
		return ServiceState{Installed: installed}, err
	}
	running, err := WindowsLocalDaemonServiceRunning(owner)
	return ServiceState{Installed: true, Running: running}, err
}

func StartCurrentUserService(ctx context.Context, _ string) error {
	owner, err := CurrentUserSID()
	if err != nil {
		return err
	}
	installed, err := WindowsLocalDaemonServiceInstalled(owner)
	if err != nil {
		return err
	}
	if !installed {
		return errors.New("managed Paperboat local daemon service is not installed; repair the Paperboat installation")
	}
	return StartWindowsOwnerService(ctx, owner)
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
