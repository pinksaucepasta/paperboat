//go:build linux || darwin

package hostinstall

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/pinksaucepasta/paperboat/internal/atomicfile"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/carriercontrol"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/updated"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

// unixCarrierInstallation belongs to the host installation lifecycle journal.
// Content pins retain the exact binary baseline without embedding an executable
// in the bounded journal. Capabilities and the ownership lock never rotate.
type unixCarrierInstallation struct {
	request     Request
	paths       installPaths
	carrierOnly bool
}
type unixCarrierBaseline struct {
	CarrierOnly bool                            `json:"carrier_only"`
	Legacy      []service.NativeComponentStatus `json:"legacy,omitempty"`
	Carrier     *CarrierConfig                  `json:"carrier,omitempty"`
	Metadata    []byte                          `json:"metadata,omitempty"`
	Worker      *installsource.Source           `json:"worker,omitempty"`
}

func deriveUnixCarrierConfig(r Request, paths installPaths) (CarrierConfig, error) {
	if r.Source.Validate() != nil {
		return CarrierConfig{}, ErrInvalidRequest
	}
	root := filepath.Dir(carrierConfigPath(paths.instance))
	c := CarrierConfig{Schema: carrierSchema, Instance: paths.instance, Source: r.Source,
		Executable: filepath.Join(filepath.Dir(paths.workerRollback), "pins", r.Source.SHA256, "pb"), UID: r.UID, GID: r.GID, User: r.User, Group: r.Group, RuntimeRoot: r.StateRoot,
		Environment: workerEnvironment(r), WorkerAddress: r.HelperListenAddress, HostdEndpoint: paths.hostdSocket, ControlEndpoint: paths.hostdSocket + ".carrier", HostdTokenFile: paths.hostdToken,
		DispatchCapabilityFile: filepath.Join(root, "dispatch.cap"), LockPath: filepath.Join(root, "owner.lock")}
	return c, c.Validate()
}

func (s unixCarrierInstallation) Capture(ctx context.Context) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	baseline := unixCarrierBaseline{CarrierOnly: s.carrierOnly}
	carrier, err := readCarrierConfigFile(ctx, s.paths.instance)
	if err == nil {
		if err = carrier.Source.Verify(carrier.Executable); err != nil {
			return nil, err
		}
		baseline.Carrier = &carrier
	} else if !installNotExistOnly(err) {
		return nil, err
	}
	previous, err := newInstallJournal(s.paths, s.request.Source)
	if err != nil {
		return nil, err
	}
	baseline.Metadata = previous.PreviousMetadata
	if previous.HadWorker {
		source := s.request.Source
		if len(baseline.Metadata) > 0 {
			installed, err := loadInstallMetadata(s.paths.metadata, s.request.UID)
			if err != nil {
				return nil, err
			}
			source = installed.Source
		}
		if err := source.Verify(s.paths.worker); err != nil {
			return nil, err
		}
		pin := filepath.Join(filepath.Dir(s.paths.workerRollback), "pins", source.SHA256, "pb")
		if _, err := os.Lstat(pin); installNotExistOnly(err) {
			if err := stageBinary(s.paths.worker, pin, source); err != nil {
				return nil, err
			}
		} else if err != nil {
			return nil, err
		}
		if err := source.Verify(pin); err != nil {
			return nil, err
		}
		baseline.Worker = &source
	}
	legacy, err := s.legacyComponents()
	if err != nil {
		return nil, err
	}
	for _, component := range legacy {
		status, err := component.Inspect(ctx)
		if err != nil {
			return nil, err
		}
		baseline.Legacy = append(baseline.Legacy, status)
	}
	return json.Marshal(baseline)
}

func (s unixCarrierInstallation) Restore(ctx context.Context, body []byte) error {
	var baseline unixCarrierBaseline
	if err := decodeInstallJSON(bytes.NewReader(body), &baseline, 256<<10); err != nil {
		return err
	}
	// A changed carrier must cease owning Tailcat before its old descriptor is
	// republished. An unchanged retained carrier keeps all accepted SSH bridges.
	current, err := readCarrierConfigFile(ctx, s.paths.instance)
	if err != nil && !installNotExistOnly(err) {
		return err
	}
	before, _ := json.Marshal(baseline.Carrier)
	now, _ := json.Marshal(func() *CarrierConfig {
		if err == nil {
			return &current
		}
		return nil
	}())
	if !bytes.Equal(before, now) {
		canonical, deriveErr := deriveUnixCarrierConfig(s.request, s.paths)
		if deriveErr != nil {
			return deriveErr
		}
		installer, deriveErr := carrierInstallerPending(canonical)
		if deriveErr != nil {
			return deriveErr
		}
		native := installerController(canonical).(service.NativeLifecycleController)
		if err := installer.Uninstall(ctx); err != nil {
			return err
		}
		status, err := native.Inspect(ctx, installer.DefinitionPath())
		if err != nil {
			return err
		}
		if status.Registered || status.Running {
			return service.ErrLifecycleUncertain
		}
		if baseline.Carrier != nil {
			c := *baseline.Carrier
			if c.Validate() != nil {
				return ErrInvalidRequest
			}
			if err := c.Source.Verify(c.Executable); err != nil {
				return err
			}
			value, err := jsonCarrier(c)
			if err != nil {
				return err
			}
			if err := atomicfile.Write(carrierConfigPath(c.Instance), value, atomicfile.Options{Mode: 0640, OwnerUID: 0, OwnerGID: c.GID}); err != nil {
				return err
			}
		} else if err := os.Remove(carrierConfigPath(s.paths.instance)); err != nil && !installNotExistOnly(err) {
			return err
		}
	}
	currentMetadata, inspectErr := newInstallJournal(s.paths, s.request.Source)
	if inspectErr != nil {
		return inspectErr
	}
	sameBinary := baseline.Worker == nil && !currentMetadata.HadWorker
	if baseline.Worker != nil {
		sameBinary = baseline.Worker.Verify(s.paths.worker) == nil
	}
	if bytes.Equal(currentMetadata.PreviousMetadata, baseline.Metadata) && sameBinary {
		if baseline.CarrierOnly {
			return nil
		}
		if err := s.restoreLegacy(ctx, baseline.Legacy); err != nil {
			return err
		}
		return errors.Join(removeInstallFile(s.paths.workerNext), removeJournal(s.paths.journal), updated.RollbackUnixNativeInstall(s.paths.updateState))
	}
	if baseline.CarrierOnly {
		return service.ErrLifecycleUncertain
	}
	hostd, _, err := pendingInstallers(s.request, s.paths)
	if err != nil {
		return err
	}
	controller, err := service.ComponentController(s.request.Platform, service.HostdKind, s.request.UID, service.ExecRunner{})
	if err != nil {
		return err
	}
	if err := controller.(service.NativeLifecycleController).Stop(ctx, hostd.DefinitionPath()); err != nil {
		return err
	}
	if baseline.Worker != nil {
		source := *baseline.Worker
		if source.Validate() != nil {
			return ErrInvalidRequest
		}
		pin := filepath.Join(filepath.Dir(s.paths.workerRollback), "pins", source.SHA256, "pb")
		if err := source.Verify(pin); err != nil {
			return err
		}
		if err := stageBinary(pin, s.paths.workerNext, source); err != nil {
			return err
		}
		if err := os.Rename(s.paths.workerNext, s.paths.worker); err != nil {
			return err
		}
	} else if err := os.Remove(s.paths.worker); err != nil && !installNotExistOnly(err) {
		return err
	}
	if len(baseline.Metadata) > 0 {
		if err := atomicfile.Write(s.paths.metadata, baseline.Metadata, atomicfile.Options{Mode: 0600, OwnerUID: 0, OwnerGID: -1}); err != nil {
			return err
		}
	} else if err := os.Remove(s.paths.metadata); err != nil && !installNotExistOnly(err) {
		return err
	}
	if err := s.restoreLegacy(ctx, baseline.Legacy); err != nil {
		return err
	}
	return errors.Join(removeInstallFile(s.paths.workerNext), removeJournal(s.paths.journal), updated.RollbackUnixNativeInstall(s.paths.updateState))
}

func stopUnixRuntimeRoles(ctx context.Context, host, hostd, updater *service.Installer) error {
	for _, installer := range []*service.Installer{updater, hostd, host} {
		if installer == nil {
			continue
		}
		component, err := installer.NativeComponent(nil)
		if err != nil {
			return err
		}
		if err := component.Stop(ctx); err != nil {
			return err
		}
	}
	return nil
}
func applyUnixNativeRoles(ctx context.Context, host, hostd, updater *service.Installer, address string, remove bool) error {
	if remove {
		for _, installer := range []*service.Installer{updater, hostd, host} {
			if installer == nil {
				continue
			}
			component, err := installer.NativeComponent(nil)
			if err != nil {
				return err
			}
			if err := component.Stop(ctx); err != nil {
				return err
			}
			if err := component.Uninstall(ctx); err != nil {
				return err
			}
		}
		return nil
	}
	for _, installer := range []*service.Installer{host, hostd, updater} {
		if installer == nil {
			continue
		}
		var probe func(context.Context) error
		if installer == hostd {
			var err error
			probe, err = service.NewHTTPReadinessProbe("http://" + address + "/healthz")
			if err != nil {
				return err
			}
		}
		component, err := installer.NativeComponent(probe)
		if err != nil {
			return err
		}
		if err := component.Install(ctx); err != nil {
			return err
		}
		if err := component.Start(ctx); err != nil {
			return err
		}
		if err := component.CheckReadiness(ctx); err != nil {
			return err
		}
	}
	return nil
}

// reconcileInstalledCarrier advances only the independently owned carrier; the
// protected installation source already represents the desired runtime.
func reconcileInstalledCarrier(ctx context.Context, instance string, _ ...func(context.Context, string) ([]string, error)) (resultErr error) {
	uid, err := carrierUID(instance)
	if err != nil {
		return err
	}
	paths := platformPaths(uid)
	lock, err := updated.LockUnixNativeInstall(ctx, paths.updateState)
	if err != nil {
		return err
	}
	if lock != nil {
		defer func() { resultErr = errors.Join(resultErr, lock.Close()) }()
	}
	request, err := LoadEnrolledOwner(uid)
	if err != nil {
		return err
	}
	request.Executable = paths.worker
	hostd, updater, err := pendingInstallers(request, paths)
	if err != nil {
		return err
	}
	var host *service.Installer
	if request.Platform != "darwin" {
		host, err = hostInstallerWithMissing(request, paths, true)
		if err != nil {
			return err
		}
	}
	lifecycle, err := newLifecycleManagerWithCarrierScope(request, paths, host, hostd, updater, true)
	if err != nil {
		return err
	}
	if err := lifecycle.RequireRecovered(ctx); err != nil {
		return hostInstallFailure("native installation recovery is required before carrier reconciliation", err)
	}
	request, err = LoadEnrolledOwner(uid)
	if err != nil {
		return err
	}
	if err := request.Source.Verify(paths.worker); err != nil {
		return err
	}
	retained, err := ReadCarrierConfig(ctx, instance)
	if err != nil {
		return err
	}
	if retained.Source == request.Source {
		return EnsureExistingCarrier(ctx, retained)
	}
	capability, err := ReadCarrierCapability(ctx, retained)
	if err != nil {
		return err
	}
	client, err := carriercontrol.NewClient(retained.ControlEndpoint, capability, retained.Source.SHA256)
	if err != nil {
		return err
	}
	if err := client.RetireSSH(ctx); errors.Is(err, carriercontrol.ErrBusy) {
		return nil
	} else if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			recovery, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
			defer cancel()
			resultErr = errors.Join(resultErr, client.ResumeSSH(recovery))
		}
	}()
	abort, err := PrepareSuppliedMaintenance(ctx, paths.hostdSocket, paths.hostdToken, false)
	if err != nil {
		return err
	}
	defer abort()
	request.Executable = paths.worker
	candidate, err := prepareCarrierConfig(ctx, request)
	if err != nil {
		return err
	}
	// Stable endpoint and account custody cannot change during a source upgrade.
	candidate.Environment = retained.Environment
	stable := candidate
	stable.Source = retained.Source
	stable.Executable = retained.Executable
	want, _ := json.Marshal(stable)
	have, _ := json.Marshal(retained)
	if !bytes.Equal(want, have) {
		return ErrInvalidRequest
	}
	resultErr = lifecycle.Replace(ctx, func(ctx context.Context) error {
		previous, err := carrierInstaller(retained)
		if err != nil {
			return err
		}
		if err := previous.Uninstall(ctx); err != nil {
			return err
		}
		native := installerController(retained).(service.NativeLifecycleController)
		state, err := native.Inspect(ctx, previous.DefinitionPath())
		if err != nil {
			return err
		}
		if state.Registered || state.Running {
			return service.ErrLifecycleUncertain
		}
		if err := publishCarrierFiles(candidate, carrierConfigPath(instance), capability); err != nil {
			return err
		}
		next, err := carrierInstaller(candidate)
		if err != nil {
			return err
		}
		if err := next.Install(ctx); err != nil {
			return err
		}
		return waitCarrierReady(ctx, candidate)
	})
	committed = resultErr == nil
	return resultErr
}

func (s unixCarrierInstallation) legacyComponents() ([]*service.NativeTransactionalComponent, error) {
	worker, err := legacyWorkerInstaller(s.request, s.paths)
	if err != nil {
		return nil, err
	}
	component, err := worker.NativeComponent(nil)
	if err != nil {
		return nil, err
	}
	result := []*service.NativeTransactionalComponent{component}
	if s.request.Platform == "darwin" {
		obsolete, err := hostInstallerWithMissing(s.request, s.paths, true)
		if err != nil {
			return nil, err
		}
		component, err := obsolete.NativeComponent(nil)
		if err != nil {
			return nil, err
		}
		result = append(result, component)
	}
	return result, nil
}

// RestorePriorNativeRuntime restarts an exact protected pre-cutover baseline
// after installation replacement has already restored its files/definitions.
// It never creates a carrier or interprets a release-version heuristic.
func RestorePriorNativeRuntime(ctx context.Context, previous Request) error {
	paths := platformPaths(previous.UID)
	installed, err := loadInstallMetadata(paths.metadata, previous.UID)
	if err != nil {
		return err
	}
	if installed.Source != previous.Source {
		return ErrInvalidRequest
	}
	if installed.SessionOwnerGeneration != "" {
		return Repair(ctx, installed)
	}
	if _, err := readCarrierConfigFile(ctx, paths.instance); err == nil {
		return ErrInvalidRequest
	} else if !installNotExistOnly(err) {
		return err
	}
	if err := installed.Source.Verify(paths.worker); err != nil {
		return err
	}
	hostd, updater, err := pendingInstallers(installed, paths)
	if err != nil {
		return err
	}
	var host *service.Installer
	if installed.Platform != "darwin" {
		host, err = hostInstallerWithMissing(installed, paths, true)
		if err != nil {
			return err
		}
	}
	for _, installer := range []*service.Installer{host, hostd, updater} {
		if installer == nil {
			continue
		}
		component, err := installer.NativeComponent(nil)
		if err != nil {
			return err
		}
		state, err := component.Inspect(ctx)
		if err != nil {
			return err
		}
		if !state.Installed {
			continue
		}
		if err := component.Start(ctx); err != nil {
			return err
		}
	}
	probe, err := service.NewHTTPReadinessProbe("http://" + installed.HelperListenAddress + "/healthz")
	if err != nil {
		return err
	}
	component, err := hostd.NativeComponent(probe)
	if err != nil {
		return err
	}
	return component.CheckReadiness(ctx)
}

func (s unixCarrierInstallation) restoreLegacy(ctx context.Context, baseline []service.NativeComponentStatus) error {
	components, err := s.legacyComponents()
	if err != nil {
		return err
	}
	if len(components) != len(baseline) {
		return service.ErrLifecycleInvalid
	}
	for index, component := range components {
		before := baseline[index]
		current, err := component.Inspect(ctx)
		if err != nil {
			return err
		}
		want, _ := json.Marshal(before)
		have, _ := json.Marshal(current)
		if bytes.Equal(want, have) {
			continue
		}
		if err := component.Restore(ctx, before); err != nil {
			return err
		}
	}
	return nil
}

// publishUnixControlLifetime runs only inside the captured native lifecycle
// transaction, before any hostd stop. It changes no carrier source or capability.
func publishUnixControlLifetime(ctx context.Context, paths installPaths, hostd *service.Installer, cold bool) error {
	if runtime.GOOS != "linux" || cold {
		return nil
	}
	c, err := readCarrierConfigFile(ctx, paths.instance)
	if err != nil {
		return err
	}
	carrier, err := carrierInstaller(c)
	if err != nil {
		return err
	}
	if err := verifyCarrierNativeCustody(c, carrier); err != nil {
		return err
	}
	if err := carrier.PublishDefinition(ctx); err != nil {
		return err
	}
	if err := hostd.PublishDefinition(ctx); err != nil {
		return err
	}
	if err := (service.ExecRunner{}).Run(ctx, "systemctl", "daemon-reload"); err != nil {
		return err
	}
	return verifyCarrierNative(c, carrier)
}
