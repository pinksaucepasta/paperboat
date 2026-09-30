//go:build windows

package hostruntimecmd

import (
	"context"
	"io"

	"github.com/pinksaucepasta/paperboat/internal/buildinfo"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostinstall"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/updated"
)

func runUpdated(ctx context.Context, args []string, _ io.Writer, stderr io.Writer) error {
	instance, err := resolveWindowsRuntimeInstance(args)
	if err != nil {
		return err
	}
	workerConfig, err := windowsUpdatedConfig(instance)
	if err != nil {
		recordWindowsServiceLaunchFailure(windowsInstanceServiceName("PaperboatUpdated", instance.name), err)
		return err
	}
	serviceName := windowsInstanceServiceName("PaperboatUpdated", instance.name)
	err = service.RunWindowsSystemServiceWithReady(serviceName, func(serviceCtx context.Context, ready func() error) error {
		runErr := updated.RunWindowsWithReady(serviceCtx, workerConfig, ready)
		if runErr != nil {
			recordWindowsServiceLaunchFailure(serviceName, runErr)
		}
		return runErr
	})
	if err != nil {
		recordWindowsServiceLaunchFailure(serviceName, err)
		if stderr != nil {
			_, _ = io.WriteString(stderr, "PaperboatUpdated startup failed: "+err.Error()+"\n")
		}
	}
	return err
}

func runActivator(_ context.Context, args []string, _ io.Writer, _ io.Writer) error {
	instance, err := resolveWindowsRuntimeInstance(args)
	if err != nil {
		return err
	}
	serviceName := windowsInstanceServiceName("PaperboatUpdateActivator", instance.name)
	config, err := windowsUpdatedConfig(instance)
	if err != nil {
		recordWindowsServiceLaunchFailure(serviceName, err)
		return err
	}
	err = service.RunWindowsSystemService(serviceName, func(serviceCtx context.Context) error {
		return updated.RunWindowsActivator(serviceCtx, config)
	})
	if err != nil {
		recordWindowsServiceLaunchFailure(serviceName, err)
	}
	return err
}

func windowsUpdatedConfig(instance windowsRuntimeInstance) (updated.WindowsConfig, error) {
	config, layout := instance.config, instance.layout
	result := windowsUpdatedConfigFor(config, layout, buildinfo.Version)
	return result, nil
}

func windowsUpdatedConfigFor(config hostinstall.WindowsRuntimeConfig, layout service.Layout, runningVersion string) updated.WindowsConfig {
	// The running signed executable is the active updater during activation.
	// runtime-install.json deliberately remains on the previous version until
	// health verification commits the transaction, so using its version here
	// makes every candidate updater report the old version and forces rollback.
	tokenFile := config.TokenFile
	installState, _ := hostinstall.WindowsInstanceConfigPath(config.Instance)
	return updated.WindowsConfig{Source: config.Source, StateRoot: layout.UpdateStateRoot, RuntimeStateRoot: config.StateRoot, Binary: layout.Binary, BinaryRollback: layout.BinaryRollback, BinaryStaged: layout.BinaryStaged, OwnerSID: config.OwnerSID, MachineID: config.MachineID, RepositoryURL: config.Artifact.RepositoryURL, TokenFile: tokenFile, InstallState: installState, ControlSocket: layout.UpdaterSocket, HostdSocket: layout.HostdSocket, HealthURL: "http://" + config.ListenAddress + "/healthz", ActiveVersion: runningVersion, Architecture: config.Artifact.Architecture, AutomaticChecks: config.Source.AutomaticUpdates, SetupMode: config.SetupMode}
}
