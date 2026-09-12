//go:build windows

package hostruntimecmd

import (
	"context"
	"io"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/buildinfo"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostdproto"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostinstall"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/updated"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/workerupdate"
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
	config, err := windowsUpdatedConfig(instance)
	if err != nil {
		return err
	}
	return service.RunWindowsSystemService(windowsInstanceServiceName("PaperboatUpdateActivator", instance.name), func(serviceCtx context.Context) error {
		return updated.RunWindowsActivator(serviceCtx, config)
	})
}

func windowsUpdatedConfig(instance windowsRuntimeInstance) (updated.WindowsConfig, error) {
	config, layout := instance.config, instance.layout
	result := windowsUpdatedConfigFor(config, layout, buildinfo.Version)
	token, err := readWindowsHostdTokenForSID(config.TokenFile, config.OwnerSID)
	if err != nil {
		return updated.WindowsConfig{}, err
	}
	client, err := hostdproto.NewClient(layout.HostdSocket, token, 31*time.Minute)
	clear(token)
	if err != nil {
		return updated.WindowsConfig{}, err
	}
	result.ActivationGate, err = workerupdate.NewDeploymentActivationGate(workerupdate.DeploymentActivationGateConfig{Provider: workerupdate.HostdDeploymentProvider{Client: client}})
	if err != nil {
		return updated.WindowsConfig{}, err
	}
	return result, nil
}

func windowsUpdatedConfigFor(config hostinstall.WindowsRuntimeConfig, layout service.Layout, runningVersion string) updated.WindowsConfig {
	// The running signed executable is the active updater during activation.
	// runtime-install.json deliberately remains on the previous version until
	// health verification commits the transaction, so using its version here
	// makes every candidate updater report the old version and forces rollback.
	tokenFile := config.TokenFile
	installState, _ := hostinstall.WindowsInstanceConfigPath(config.Instance)
	return updated.WindowsConfig{StateRoot: layout.UpdateStateRoot, RuntimeStateRoot: config.StateRoot, Binary: layout.Binary, BinaryRollback: layout.BinaryRollback, BinaryStaged: layout.BinaryStaged, OwnerSID: config.OwnerSID, MachineID: config.MachineID, RepositoryURL: config.Artifact.RepositoryURL, TokenFile: tokenFile, InstallState: installState, ControlSocket: layout.UpdaterSocket, HostdSocket: layout.HostdSocket, HealthURL: "http://" + config.ListenAddress + "/healthz", ActiveVersion: runningVersion, Architecture: config.Artifact.Architecture, AutomaticActivation: true, SetupMode: config.SetupMode,
		CandidateStarter: func(ctx context.Context, request workerupdate.StartRequest) (workerupdate.Worker, error) {
			return startWindowsRuntimeWorkerForRelease(ctx, request.Executable, request.HostdEndpoint, tokenFile, config.OwnerSID, request.WorkerID, request.Release.Version, request.Release.HostdAPIMin, request.Release.HostdAPIMax)
		},
	}
}
