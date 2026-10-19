//go:build darwin || linux

package hostinstall

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strconv"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/environmentkey"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
)

func installers(request Request, paths installPaths) (*service.Installer, *service.Installer, error) {
	return installersWithMissing(request, paths, false)
}

func pendingInstallers(request Request, paths installPaths) (*service.Installer, *service.Installer, error) {
	return installersWithMissing(request, paths, true)
}

func installersWithMissing(request Request, paths installPaths, allowMissingExecutable bool) (*service.Installer, *service.Installer, error) {
	return installersWithMaintenance(request, paths, allowMissingExecutable, false)
}

func installersWithMaintenance(request Request, paths installPaths, allowMissingExecutable, maintenancePending bool) (*service.Installer, *service.Installer, error) {
	layout, err := componentLayout(paths)
	if err != nil {
		return nil, nil, err
	}
	hostdController, err := service.ComponentController(request.Platform, service.HostdKind, request.UID, service.ExecRunner{})
	if err != nil {
		return nil, nil, err
	}
	hostdConfig := service.ComponentConfig{
		Layout: layout, User: request.User, Group: request.Group, UID: request.UID, GID: request.GID,
		HostdTokenFile: paths.hostdToken, Environment: workerEnvironment(request), Controller: hostdController,
	}
	if request.Source.Validate() == nil {
		sourceBody, err := json.Marshal(request.Source)
		if err != nil {
			return nil, nil, err
		}
		hostdConfig.Environment["PAPERBOAT_INSTALL_SOURCE"] = base64.RawStdEncoding.EncodeToString(sourceBody)
	}
	if maintenancePending {
		hostdConfig.Environment["PAPERBOAT_NATIVE_UPDATE_PENDING"] = "true"
	}
	if request.Platform == "linux" {
		hostdConfig.EncryptedCredentials = map[string]string{environmentkey.CredentialName: paths.environmentCredential}
	}
	var hostd *service.Installer
	if allowMissingExecutable {
		hostd, err = service.NewHostdInstallerPending(hostdConfig)
	} else {
		hostd, err = service.NewHostdInstaller(hostdConfig)
	}
	if err != nil {
		return nil, nil, err
	}
	updaterController, err := service.ComponentController(request.Platform, service.UpdaterKind, request.UID, service.ExecRunner{})
	if err != nil {
		return nil, nil, err
	}
	updaterConfig := service.ComponentConfig{
		Layout: layout, User: request.User, Group: request.Group, UID: request.UID, GID: request.GID,
		HostdTokenFile: paths.hostdToken, ReleaseRepository: request.Artifact.RepositoryURL, MachineID: request.UserMachineID, Source: request.Source,
		HealthURL: "http://" + request.HelperListenAddress + "/healthz", Controller: updaterController,
	}
	var updater *service.Installer
	if allowMissingExecutable {
		updater, err = service.NewUpdaterInstallerPending(updaterConfig)
	} else {
		updater, err = service.NewUpdaterInstaller(updaterConfig)
	}
	if err != nil {
		return nil, nil, err
	}
	return hostd, updater, nil
}

func newLifecycleManager(request Request, paths installPaths, host, hostd, updater *service.Installer) (*service.LifecycleManager, error) {
	return newLifecycleManagerWithCarrierScope(request, paths, host, hostd, updater, false)
}

func newLifecycleManagerWithCarrierScope(request Request, paths installPaths, host, hostd, updater *service.Installer, carrierOnly bool) (*service.LifecycleManager, error) {
	return newLifecycleManagerWithRestorePolicy(request, paths, host, hostd, updater, carrierOnly, nil)
}

func newLifecycleManagerWithRestorePolicy(request Request, paths installPaths, host, hostd, updater *service.Installer, carrierOnly bool, authorize func(context.Context, installsource.Source, string) error) (*service.LifecycleManager, error) {
	if hostd == nil || updater == nil {
		return nil, service.ErrLifecycleInvalid
	}
	probe, err := service.NewHTTPReadinessProbe("http://" + request.HelperListenAddress + "/healthz")
	if err != nil {
		return nil, err
	}
	carrierConfig, err := deriveUnixCarrierConfig(request, paths)
	if err != nil {
		return nil, err
	}
	retained, readErr := readCarrierConfigFile(context.Background(), paths.instance)
	if readErr == nil {
		carrierConfig = retained
	} else if !installNotExistOnly(readErr) {
		return nil, readErr
	}
	carrier, err := carrierInstallerPending(carrierConfig)
	if err != nil {
		return nil, err
	}
	var installation service.NativeInstallationState = unixCarrierInstallation{request: request, paths: paths, carrierOnly: carrierOnly}
	if authorize != nil {
		installation = preparedUnixInstallation{state: installation, paths: paths, authorize: authorize}
	}
	return service.NewHostLifecycleManager(service.HostLifecycleConfig{
		Carrier: carrier,
		CarrierProbe: func(ctx context.Context) error {
			current, err := readCarrierConfigFile(ctx, paths.instance)
			if err != nil {
				return err
			}
			return waitCarrierReady(ctx, current)
		},
		NativeInstallation: installation,
		StateRoot:          filepath.Join(paths.root, "service-lifecycle"),
		Host:               host,
		Hostd:              hostd,
		Updater:            updater,
		HostdProbe:         probe,
		// updated has no public HTTP health endpoint. Its native ready state and
		// authenticated hostd/update control transaction are the readiness
		// boundary; do not invent a second loopback endpoint.
		UpdaterProbe: nil,
	})
}

func legacyWorkerInstaller(request Request, paths installPaths) (*service.Installer, error) {
	controller := service.Controller(service.SystemdController{Runner: service.ExecRunner{}})
	if runtime.GOOS == "darwin" {
		controller = service.LaunchdController{Runner: service.ExecRunner{}, UID: request.UID}
	}
	return service.NewPending(service.Config{
		Platform: request.Platform, Kind: service.WorkerKind, ConfigRoot: string(os.PathSeparator), Executable: paths.worker,
		User: request.User, Group: request.Group, Arguments: []string{"daemon", "__runtime-host"}, Controller: controller,
		Environment: workerEnvironment(request),
	})
}

func componentLayout(paths installPaths) (service.Layout, error) {
	layout, err := service.DefaultLayout(runtime.GOOS)
	if err != nil {
		return service.Layout{}, err
	}
	layout.Instance = paths.instance
	layout.InstallRoot = paths.root
	layout.ReleasesRoot = filepath.Join(paths.root, "releases")
	layout.Binary = paths.worker
	layout.BinaryRollback = paths.workerRollback
	layout.BinaryStaged = paths.workerNext
	layout.UpdateStateRoot = paths.updateState
	layout.HostdSocket = paths.hostdSocket
	if err := layout.Validate(); err != nil {
		return service.Layout{}, err
	}
	return layout, nil
}

func hostInstaller(request Request, paths installPaths) (*service.Installer, error) {
	return hostInstallerWithMissing(request, paths, false)
}

func hostInstallerWithMissing(request Request, paths installPaths, allowMissingExecutable bool) (*service.Installer, error) {
	instance := "u" + strconv.Itoa(request.UID)
	hostController := service.Controller(service.SystemdController{Runner: service.ExecRunner{}, Unit: "paperboat-runtime-privileged-" + instance + ".service"})
	rootGroup := "root"
	if runtime.GOOS == "darwin" {
		rootGroup = "wheel"
		hostController = service.LaunchdController{Runner: service.ExecRunner{}, UID: request.UID, Label: service.HostLabel + "." + instance}
	}
	config := service.Config{
		Platform: request.Platform, Kind: service.HostKind, Instance: instance, ConfigRoot: string(os.PathSeparator), Executable: paths.worker,
		User: "root", Group: rootGroup, Arguments: []string{
			"daemon", "__runtime-host-service", "--uid", strconv.Itoa(request.UID), "--gid", strconv.Itoa(request.GID),
			"--listen-address", request.HelperListenAddress,
		}, Controller: hostController,
	}
	var host *service.Installer
	var err error
	if allowMissingExecutable {
		host, err = service.NewPending(config)
	} else {
		host, err = service.New(config)
	}
	if err != nil {
		return nil, err
	}
	return host, nil
}

type installPaths struct {
	instance                                             string
	root, installerState, runtimeState                   string
	worker, workerNext, workerRollback, workerPrevious   string
	journal, metadata                                    string
	hostdToken, hostdSocket, updaterSocket, updateState  string
	environmentCredentialDirectory                       string
	environmentCredential, environmentCredentialMetadata string
}
