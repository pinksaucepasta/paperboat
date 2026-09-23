//go:build windows

package daemoncmd

import (
	"errors"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostinstall"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
	"golang.org/x/sys/windows"
	"os"
	"path/filepath"
	"strings"
)

const windowsConfigWorkloadEnvironment = "PAPERBOAT_WINDOWS_CONFIG_WORKLOAD"

// enterWindowsConfigService turns the LocalSystem SCM invocation into the
// enrolled user's worker. The child marker is generated only by this function;
// direct callers cannot select another SID or state root.
func enterWindowsConfigService(stateRoot string) (bool, error) {
	install, err := hostinstall.LoadWindowsRuntimeConfig()
	if err != nil {
		return false, err
	}
	if stateRoot != install.StateRoot {
		return false, errors.New("Paperboat Windows config sync state root does not match the installed owner")
	}
	if os.Getenv(windowsConfigWorkloadEnvironment) == "1" {
		if !ownerSIDMatches(install.OwnerSID) {
			return false, errors.New("Paperboat Windows config sync worker is not running as the enrolled owner")
		}
		return false, nil
	}
	layout, err := service.DefaultLayout("windows")
	if err != nil {
		return false, err
	}
	err = service.RunWindowsService(service.ServiceEntryConfig{
		Name:        "PaperboatRuntimeConfig",
		Executable:  layout.Binary,
		Arguments:   []string{"daemon", "__runtime-config", "--state-root", install.StateRoot},
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

func ownerSIDMatches(ownerSID string) bool {
	want, err := windows.StringToSid(ownerSID)
	if err != nil || want == nil || !want.IsValid() {
		return false
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	return err == nil && user != nil && user.User.Sid != nil && user.User.Sid.Equals(want)
}

func defaultChezmoiPath() string {
	programFiles := strings.TrimSpace(os.Getenv("ProgramFiles"))
	if programFiles == "" {
		programFiles = `C:\Program Files`
	}
	return filepath.Join(programFiles, "Paperboat", "chezmoi.exe")
}
