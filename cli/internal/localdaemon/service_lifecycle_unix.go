//go:build darwin || linux

package localdaemon

import (
	"context"
	"os"
	"runtime"

	hostservice "github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
)

// CreateCurrentUserService is pb service install. Linux and macOS install the unit the way a
// command that finds no daemon does; Windows needs administrator rights to register its service.
func CreateCurrentUserService(ctx context.Context, executable, configPath, serverURL string) error {
	return InstallCurrentUserService(ctx, executable, configPath, serverURL)
}

func UninstallCurrentUserService(ctx context.Context, executable string) error {
	return RemoveCurrentUserService(ctx, executable)
}

func currentUserLifecycle(executable string) (hostservice.NativeLifecycleController, string, error) {
	config, err := currentUserServiceConfig(executable)
	if err != nil {
		return nil, "", err
	}
	definition, err := serviceDefinition(config)
	if err != nil {
		return nil, "", err
	}
	controller, ok := definition.Controller.(hostservice.NativeLifecycleController)
	if !ok {
		return nil, "", hostservice.ErrUnsupportedPlatform
	}
	installer, err := hostservice.New(definition)
	if err != nil {
		return nil, "", err
	}
	return controller, installer.DefinitionPath(), nil
}

func InspectCurrentUserService(ctx context.Context, executable string) (ServiceState, error) {
	controller, path, err := currentUserLifecycle(executable)
	if err != nil {
		return ServiceState{}, err
	}
	state, err := controller.Inspect(ctx, path)
	if err != nil {
		return ServiceState{}, err
	}
	installed := state.Registered
	if runtime.GOOS == "darwin" {
		installed, err = darwinServiceInstalled(path, executable, os.Geteuid())
	}
	return ServiceState{Installed: installed, Running: state.Running}, err
}

func StartCurrentUserService(ctx context.Context, executable string) error {
	controller, path, err := currentUserLifecycle(executable)
	if err != nil {
		return err
	}
	return startUserService(ctx, controller, path, runtime.GOOS, executable, os.Geteuid())
}

func startUserService(ctx context.Context, controller hostservice.NativeLifecycleController, path, platform, executable string, uid int) error {
	if platform == "darwin" {
		installed, err := darwinServiceInstalled(path, executable, uid)
		if err != nil {
			return err
		}
		if !installed {
			return ErrServiceNotInstalled
		}
		return controller.Start(ctx, path)
	}
	state, err := controller.Inspect(ctx, path)
	if err != nil {
		return err
	}
	if !state.Registered {
		return ErrServiceNotInstalled
	}
	return controller.Start(ctx, path)
}

func StopCurrentUserService(ctx context.Context, executable string) error {
	controller, path, err := currentUserLifecycle(executable)
	if err != nil {
		return err
	}
	state, err := controller.Inspect(ctx, path)
	if err != nil || !state.Registered || !state.Running {
		return err
	}
	return controller.Stop(ctx, path)
}
