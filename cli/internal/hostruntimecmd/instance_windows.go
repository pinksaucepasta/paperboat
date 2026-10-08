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

func currentWindowsRuntimeInstance() (windowsRuntimeInstance, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil || user == nil || user.User.Sid == nil {
		return windowsRuntimeInstance{}, errors.New("Windows runtime user is unavailable")
	}
	instance, err := service.WindowsUserInstance(user.User.Sid.String())
	if err != nil {
		return windowsRuntimeInstance{}, err
	}
	return resolveWindowsRuntimeInstance([]string{"--instance", instance})
}
