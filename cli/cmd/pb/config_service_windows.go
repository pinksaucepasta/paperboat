//go:build windows

package main

import (
	"context"
	"errors"

	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/hostinstall"
	"github.com/pinksaucepasta/paperboat/cli/internal/windows/elevation"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

// manageWindowsConfigService uses the same protected elevation bridge as install.
func manageWindowsConfigService(ctx context.Context, stateRoot string, install bool) (bool, error) {
	user, err := windows.GetCurrentThreadEffectiveToken().GetTokenUser()
	if err != nil {
		return true, err
	}
	owner := user.User.Sid.String()
	instance, err := hostinstall.WindowsInstanceForSID(owner)
	if err != nil {
		return true, err
	}
	config, err := hostinstall.LoadWindowsRuntimeConfigForInstance(instance)
	if err != nil {
		return true, err
	}
	if stateRoot != config.StateRoot || !ownerSIDMatches(config.OwnerSID) {
		return true, errors.New("Paperboat Windows config sync must be managed by the enrolled owner")
	}
	definition, err := hostinstall.WindowsConfigServiceDefinition(owner)
	if err != nil {
		return true, err
	}
	if install && windowsConfigServiceStatus() == "active" {
		// Re-applies run unelevated from the daemon; a running service needs nothing.
		return true, nil
	}
	if elevation.IsCurrentProcessElevated() {
		// The bridge cannot break away from an OpenSSH session's job; an
		// elevated pb needs no bridge.
		return true, hostinstall.ManageWindowsConfigService(ctx, owner, install)
	}
	action := elevation.ActionConfigRemove
	if install {
		action = elevation.ActionConfigInstall
	}
	return true, elevation.RunRuntimeService(ctx, definition.Executable, action, nil)
}

func ownerSIDMatches(ownerSID string) bool {
	want, err := windows.StringToSid(ownerSID)
	if err != nil || want == nil || !want.IsValid() {
		return false
	}
	user, err := windows.GetCurrentThreadEffectiveToken().GetTokenUser()
	return err == nil && user != nil && user.User.Sid != nil && user.User.Sid.Equals(want)
}

func windowsConfigServiceStatus() string {
	// Status queries must work in the enrolled user's non-elevated terminal.
	// mgr.Connect/OpenService request ALL_ACCESS even for a read-only query.
	handle, err := windows.OpenSCManager(nil, nil, windows.SC_MANAGER_CONNECT)
	if err != nil {
		return "unavailable"
	}
	manager := &mgr.Mgr{Handle: handle}
	defer manager.Disconnect()
	user, err := windows.GetCurrentThreadEffectiveToken().GetTokenUser()
	if err != nil {
		return "unavailable"
	}
	instance, err := hostinstall.WindowsInstanceForSID(user.User.Sid.String())
	if err != nil {
		return "invalid"
	}
	return windowsServiceState(manager, "PaperboatRuntimeConfig-"+instance)
}

// windowsServiceState is not_installed, installed_inactive, active, invalid or unavailable for serviceName,
// queried with read-only rights.
func windowsServiceState(manager *mgr.Mgr, serviceName string) string {
	name, err := windows.UTF16PtrFromString(serviceName)
	if err != nil {
		return "invalid"
	}
	serviceHandle, err := windows.OpenService(manager.Handle, name, windows.SERVICE_QUERY_STATUS)
	if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		return "not_installed"
	}
	if err != nil {
		return "unavailable"
	}
	configService := &mgr.Service{Name: serviceName, Handle: serviceHandle}
	defer configService.Close()
	status, err := configService.Query()
	if err != nil {
		return "unavailable"
	}
	if status.State == svc.Running {
		return "active"
	}
	return "installed_inactive"
}
