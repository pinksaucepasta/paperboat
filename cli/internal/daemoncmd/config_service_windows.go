//go:build windows

package daemoncmd

import (
	"errors"
	"os"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostinstall"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
	"golang.org/x/sys/windows"
)

const windowsConfigWorkloadEnvironment = "PAPERBOAT_WINDOWS_CONFIG_WORKLOAD"

func resolveWindowsConfigStateRoot(stateRoot, instance string) (string, error) {
	install, err := hostinstall.LoadWindowsRuntimeConfigForInstance(instance)
	if err != nil {
		return "", err
	}
	if stateRoot != "" && stateRoot != install.StateRoot {
		return "", errors.New("Paperboat Windows config sync state root does not match the installed owner")
	}
	return install.StateRoot, nil
}

// enterWindowsConfigService turns the LocalSystem SCM invocation into the
// enrolled user's worker. The child marker is generated only by this function;
// direct callers cannot select another SID or state root.
func enterWindowsConfigService(stateRoot, instance string) (bool, error) {
	install, err := hostinstall.LoadWindowsRuntimeConfigForInstance(instance)
	if err != nil {
		return false, err
	}
	if stateRoot != install.StateRoot {
		return false, errors.New("Paperboat Windows config sync state root does not match the installed owner")
	}
	if os.Getenv(windowsConfigWorkloadEnvironment) == "1" {
		matches, ownerErr := ownerSIDMatches(install.OwnerSID)
		if ownerErr != nil {
			return false, ownerErr
		}
		if !matches {
			return false, coordinatorFailure("Paperboat Windows config sync worker is not running as the enrolled owner", os.ErrPermission)
		}
		return false, nil
	}
	layout, err := hostinstall.WindowsLayoutForInstance(instance)
	if err != nil {
		return false, err
	}
	err = service.RunWindowsService(service.ServiceEntryConfig{
		Name:        "PaperboatRuntimeConfig-" + instance,
		Executable:  layout.Binary,
		Arguments:   []string{"daemon", "__runtime-config", "--instance", instance},
		EnrolledSID: install.OwnerSID,
		Environment: map[string]string{
			windowsConfigWorkloadEnvironment:  "1",
			"PAPERBOAT_WINDOWS_OWNER_SID":     install.OwnerSID,
			"PAPERBOAT_RUNTIME_STATE_ROOT":    install.StateRoot,
			"PAPERBOAT_RUNTIME_SERVICE_SCOPE": "user",
		},
	})
	return true, err
}

func ownerSIDMatches(ownerSID string) (bool, error) {
	want, err := windows.StringToSid(ownerSID)
	if err != nil {
		return false, coordinatorFailure("Paperboat Windows config sync owner could not be resolved", err)
	}
	if want == nil || !want.IsValid() {
		return false, errors.New("Paperboat Windows config sync owner is invalid")
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return false, coordinatorFailure("Paperboat Windows config sync process owner could not be queried", err)
	}
	if user == nil || user.User.Sid == nil {
		return false, errors.New("Paperboat Windows config sync process owner is unavailable")
	}
	return user.User.Sid.Equals(want), nil
}
