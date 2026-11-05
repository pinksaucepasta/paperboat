//go:build darwin || linux

package localdaemon

import (
	"context"
	"errors"
	"os"
	"runtime"

	hostservice "github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
)

func UninstallCurrentUserService(ctx context.Context, executable string) error {
	return RemoveCurrentUserService(ctx, executable)
}

// errNoGeneration means no daemon generation is installed for this user.
var errNoGeneration = errors.New("Paperboat local daemon service is not installed; run pb service install")

// currentUserLifecycle addresses the newest installed daemon generation: the one the last install started.
func currentUserLifecycle(executable string) (hostservice.NativeLifecycleController, string, error) {
	config, err := currentUserServiceConfig(executable)
	if err != nil {
		return nil, "", err
	}
	generations, err := installedGenerations(config.Platform, config.Home)
	if err != nil {
		return nil, "", err
	}
	if len(generations) == 0 {
		return nil, "", errNoGeneration
	}
	config.Generation = generations[0]
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
	if errors.Is(err, errNoGeneration) {
		return ServiceState{}, nil
	}
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
			return errNoGeneration
		}
		return controller.Start(ctx, path)
	}
	state, err := controller.Inspect(ctx, path)
	if err != nil {
		return err
	}
	if !state.Registered {
		return errNoGeneration
	}
	return controller.Start(ctx, path)
}

func StopCurrentUserService(ctx context.Context, executable string) error {
	controller, path, err := currentUserLifecycle(executable)
	if errors.Is(err, errNoGeneration) {
		return nil
	}
	if err != nil {
		return err
	}
	state, err := controller.Inspect(ctx, path)
	if err != nil || !state.Registered || !state.Running {
		return err
	}
	return controller.Stop(ctx, path)
}
