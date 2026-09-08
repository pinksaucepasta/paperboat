//go:build windows

package main

import (
	"errors"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostinstall"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

// windowsConfigServiceDefinition deliberately uses the fixed machine runtime
// instead of the caller's pb path. The SCM service is a token bridge and never
// executes the config worker as LocalSystem.
func windowsConfigServiceDefinition(stateRoot string) (service.Config, bool, error) {
	install, err := hostinstall.LoadWindowsRuntimeConfig()
	if err != nil {
		return service.Config{}, true, err
	}
	if stateRoot != install.StateRoot || !ownerSIDMatches(install.OwnerSID) {
		return service.Config{}, true, errors.New("Paperboat Windows config sync must be managed by the enrolled owner")
	}
	layout, err := service.DefaultLayout("windows")
	if err != nil {
		return service.Config{}, true, err
	}
	return service.Config{
		Platform: "windows", Kind: service.ConfigKind, ConfigRoot: hostinstall.WindowsProgramDataRoot(),
		Executable: layout.Binary, User: "Paperboat", Group: "Paperboat",
		Arguments:  []string{"daemon", "__runtime-config", "--state-root", install.StateRoot},
		Controller: service.WindowsController{},
	}, true, nil
}

func ownerSIDMatches(ownerSID string) bool {
	want, err := windows.StringToSid(ownerSID)
	if err != nil || want == nil || !want.IsValid() {
		return false
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	return err == nil && user != nil && user.User.Sid != nil && user.User.Sid.Equals(want)
}

func windowsConfigServiceStatus() string {
	manager, err := mgr.Connect()
	if err != nil {
		return "invalid"
	}
	defer manager.Disconnect()
	configService, err := manager.OpenService("PaperboatRuntimeConfig")
	if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		return "not_installed"
	}
	if err != nil {
		return "invalid"
	}
	defer configService.Close()
	status, err := configService.Query()
	if err != nil {
		return "invalid"
	}
	if status.State == svc.Running {
		return "active"
	}
	return "installed_inactive"
}
