//go:build windows

package hostruntimecmd

import (
	"errors"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostinstall"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
	"golang.org/x/sys/windows"
)

type windowsRuntimeInstance struct {
	name   string
	config hostinstall.WindowsRuntimeConfig
	layout service.Layout
}

func resolveWindowsRuntimeInstance(args []string) (windowsRuntimeInstance, error) {
	if len(args) != 2 || args[0] != "--instance" {
		return windowsRuntimeInstance{}, errors.New("Windows runtime service requires --instance")
	}
	config, err := hostinstall.LoadWindowsRuntimeConfigForInstance(args[1])
	if err != nil {
		return windowsRuntimeInstance{}, err
	}
	layout, err := hostinstall.WindowsLayoutForInstance(args[1])
	if err != nil {
		return windowsRuntimeInstance{}, err
	}
	return windowsRuntimeInstance{name: args[1], config: config, layout: layout}, nil
}

func windowsInstanceServiceName(base, instance string) string { return base + "-" + instance }

var recordServiceEntryFailure = recordWindowsServiceLaunchFailure

// resolveWindowsServiceInstance is resolveWindowsRuntimeInstance for a service's entry point. Under the service
// manager nobody reads stderr: a service that cannot read its own installation exited and left only "did not respond
// to the start request" (winpool 2026-11-10). The cause goes to that service's log, which the account's uninstall
// removes; a name that is not an instance has no installation and no log.
func resolveWindowsServiceInstance(service string, args []string) (windowsRuntimeInstance, error) {
	instance, err := resolveWindowsRuntimeInstance(args)
	if err != nil && len(args) == 2 {
		if _, rootErr := hostinstall.WindowsInstanceRoot(args[1]); rootErr == nil {
			recordServiceEntryFailure(windowsInstanceServiceName(service, args[1]), err)
		}
	}
	return instance, err
}

func currentWindowsRuntimeInstance() (windowsRuntimeInstance, error) {
	user, err := windows.GetCurrentThreadEffectiveToken().GetTokenUser()
	if err != nil || user == nil || user.User.Sid == nil {
		return windowsRuntimeInstance{}, runtimeFailure("Windows runtime user is unavailable", err)
	}
	instance, err := service.WindowsUserInstance(user.User.Sid.String())
	if err != nil {
		return windowsRuntimeInstance{}, err
	}
	return resolveWindowsRuntimeInstance([]string{"--instance", instance})
}
