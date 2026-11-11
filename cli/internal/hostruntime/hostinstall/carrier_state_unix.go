//go:build linux || darwin

package hostinstall

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/pinksaucepasta/paperboat/internal/atomicfile"
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
	// Candidate is the source being installed. The journal outlives the request that made it: a recovery starts
	// from the installed record, so the candidate's carrier, hostd and executable are named here, never derived.
	Candidate installsource.Source `json:"candidate"`
}

func deriveUnixCarrierConfig(r Request, paths installPaths) (CarrierConfig, error) {
	generation, err := sourceGeneration(r.Source)
	if err != nil {
		return CarrierConfig{}, err
	}
	root := filepath.Dir(carrierConfigPath(paths.instance))
	c := CarrierConfig{Schema: carrierSchema, Instance: paths.instance, Source: r.Source, Generation: generation,
		Executable: filepath.Join(filepath.Dir(paths.workerRollback), "pins", r.Source.SHA256, "pb"), UID: r.UID, GID: r.GID, User: r.User, Group: r.Group, RuntimeRoot: r.StateRoot,
		Environment: workerEnvironment(r), WorkerAddress: r.HelperListenAddress, HostdEndpoint: paths.hostdSocket, ControlEndpoint: carrierControlEndpoint(paths.hostdSocket, generation), HostdTokenFile: paths.hostdToken,
		DispatchCapabilityFile: filepath.Join(root, "dispatch.cap"), LockPath: filepath.Join(root, "owner-"+generation+".lock")}
	return c, c.Validate()
}

func (s unixCarrierInstallation) Capture(ctx context.Context) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	baseline := unixCarrierBaseline{CarrierOnly: s.carrierOnly, Candidate: s.request.Source}
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

// restoreBaselineCarrier brings the previous generation back as the serving one. Its descriptor names it active before
// its process starts: a carrier started while the failed candidate is still the active generation waits as a standby
// for a network handover that never comes, and the machine stays offline (pbvm4 rollback injection, 31 s exec failures
// for minutes after "previous version restored").
func restoreBaselineCarrier(ctx context.Context, baseline CarrierConfig, publish func(CarrierConfig) error, start func(context.Context, CarrierConfig) error) error {
	if err := publish(baseline); err != nil {
		return err
	}
	return start(ctx, baseline)
}

func (s unixCarrierInstallation) Restore(ctx context.Context, body []byte) (resultErr error) {
	var removeErr error
	defer func() { resultErr = errors.Join(removeErr, resultErr) }()
	var baseline unixCarrierBaseline
	if err := decodeInstallJSON(bytes.NewReader(body), &baseline, 256<<10); err != nil {
		return err
	}
	if baseline.Candidate.Validate() != nil {
		return ErrInvalidRequest
	}
	s.request.Source = baseline.Candidate
	if err := s.verifyBaselineBytes(baseline); err != nil {
		return err // before anything is stopped or rewritten: a tampered pin changes nothing
	}
	// The candidate generation is removed; the baseline generation never stopped
	// serving and keeps all accepted streams. Only its descriptor is put back.
	canonical, err := deriveUnixCarrierConfig(s.request, s.paths)
	if err != nil {
		return err
	}
	if baseline.Carrier == nil || baseline.Carrier.Generation != canonical.Generation {
		// A candidate that cannot be removed must not leave the baseline unrestored: restore, then report both.
		removeErr = removeCarrierGeneration(ctx, canonical)
		// A failed network handover may have released the previous carrier's network and exited it.
		if baseline.Carrier != nil {
			if baseline.Carrier.Validate() != nil {
				return ErrInvalidRequest
			}
			if err := baseline.Carrier.Source.Verify(baseline.Carrier.Executable); err != nil {
				return err
			}
			if err := restoreBaselineCarrier(ctx, *baseline.Carrier, publishCarrierDescriptor, startCarrierIfStopped); err != nil {
				return err
			}
		}
	}
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
		if baseline.Carrier != nil {
			c := *baseline.Carrier
			if c.Validate() != nil {
				return ErrInvalidRequest
			}
			if err := c.Source.Verify(c.Executable); err != nil {
				return err
			}
			if err := publishCarrierDescriptor(c); err != nil {
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
		return s.finishFileRollback()
	}
	if baseline.CarrierOnly {
		return service.ErrLifecycleUncertain
	}
	// The candidate hostd generation may hold the handed-over listeners: stop it
	// before the baseline generation restarts on the restored executable below.
	hostd, _, err := pendingInstallers(s.request, s.paths)
	if err != nil {
		return err
	}
	candidateHostd, err := hostd.NativeComponent(nil)
	if err != nil {
		return err
	}
	if err := candidateHostd.Stop(ctx); err != nil {
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
	if err := startBaselineHostd(ctx, s.request, baseline.Worker); err != nil {
		return err
	}
	return s.finishFileRollback()
}

// verifyBaselineBytes proves the bytes a restore would put back are the baseline's own: the worker's retained pin
// (a carrier-only restore keeps the installed worker) and the baseline carrier's executable.
func (s unixCarrierInstallation) verifyBaselineBytes(baseline unixCarrierBaseline) error {
	if baseline.Worker != nil && !baseline.CarrierOnly {
		source := *baseline.Worker
		if source.Validate() != nil {
			return ErrInvalidRequest
		}
		if err := source.Verify(filepath.Join(filepath.Dir(s.paths.workerRollback), "pins", source.SHA256, "pb")); err != nil {
			return err
		}
	}
	if baseline.Carrier != nil {
		if baseline.Carrier.Validate() != nil {
			return ErrInvalidRequest
		}
		return baseline.Carrier.Source.Verify(baseline.Carrier.Executable)
	}
	return nil
}

// finishFileRollback ends a restore. The journal goes last and only after the native rollback succeeded: a
// failure here must leave it for the next start to retry (without it nothing retries, and every later install is
// refused as pending).
func (s unixCarrierInstallation) finishFileRollback() error {
	if err := updated.RollbackUnixNativeInstall(s.paths.updateState); err != nil {
		return err
	}
	if err := removeInstallFile(s.paths.workerNext); err != nil {
		return err
	}
	return removeJournal(s.paths.journal)
}

// startBaselineHostd restarts the previous hostd generation if it handed its
// listeners over and exited; a still serving one is left alone.
func startBaselineHostd(ctx context.Context, request Request, source *installsource.Source) error {
	if source == nil || source.SHA256 == request.Source.SHA256 {
		return nil
	}
	generation, err := sourceGeneration(*source)
	if err != nil {
		return err
	}
	controller, err := service.HostdController(request.Platform, generation, request.UID, service.ExecRunner{})
	if err != nil {
		return err
	}
	native := controller.(service.NativeLifecycleController)
	definition := hostdGenerationDefinition(request.Platform, request.UID, generation)
	status, err := native.Inspect(ctx, definition)
	if err != nil || !status.Registered || status.Running {
		return err
	}
	return native.Start(ctx, definition)
}

// Stage the verified replacement while the current runtime can still serve.
// Only promotion and service activation belong in the stopped interval.
func stageAndStopUnixRuntimeRoles(ctx context.Context, request Request, paths installPaths, host, hostd, updater *service.Installer, prepare func(context.Context) error) error {
	if err := stageBinary(request.Executable, paths.workerNext, request.Source); err != nil {
		return err
	}
	if host != nil {
		if err := request.Source.Verify(host.ExecutablePath()); err != nil {
			return err
		}
	}
	return prepareUnixRuntimeCutover(ctx, host, hostd, updater, prepare)
}

// prepareUnixRuntimeCutover owns native service work at the admission boundary.
func prepareUnixRuntimeCutover(ctx context.Context, host, hostd, updater *service.Installer, prepare func(context.Context) error) error {
	if host != nil {
		if err := host.Install(ctx); err != nil {
			return err
		}
	}
	// systemd publication does not start a process. Prepare declarations while
	// the old hostd still serves; launchd bootstrap starts absent jobs, so its
	// declarations remain in the activation phase.
	if runtime.GOOS == "linux" {
		for _, installer := range []*service.Installer{hostd, updater} {
			if installer == nil {
				continue
			}
			component, err := installer.NativeComponent(nil)
			if err != nil {
				return err
			}
			if err := component.Install(ctx); err != nil {
				return err
			}
		}
	}
	if err := prepare(ctx); err != nil {
		return err
	}
	return stopUnixRuntimeRoles(ctx, nil, hostd, updater)
}

// stopUnixRuntimeRoles stops roles in dependency order. hostd is a generation's
// own unit: on an update it names the candidate, which is not running, so the
// serving generation keeps running until it hands its listeners over.
// Resolve every component before stopping any serving process.
func stopUnixRuntimeRoles(ctx context.Context, host, hostd, updater *service.Installer) error {
	components := make([]*service.NativeTransactionalComponent, 0, 3)
	for _, installer := range []*service.Installer{updater, hostd, host} {
		if installer == nil {
			continue
		}
		component, err := installer.NativeComponent(nil)
		if err != nil {
			return err
		}
		components = append(components, component)
	}
	for _, component := range components {
		if err := component.Stop(ctx); err != nil {
			return err
		}
	}
	return nil
}
func applyUnixNativeRoles(ctx context.Context, host, hostd, updater *service.Installer, address string) error {
	return activateUnixNativeRoles(ctx, host, hostd, updater, address, true)
}

// activateUnixNativeRoles starts the roles. A new hostd generation imports the
// serving generation's listeners (PAPERBOAT_HOSTD_HANDOVER_SOCKET) and reports
// ready to systemd only once it serves them.
func activateUnixNativeRoles(ctx context.Context, host, hostd, updater *service.Installer, address string, publish bool) error {
	// Terminal recovery must not wait for the independent power-policy service.
	for _, installer := range []*service.Installer{hostd, host, updater} {
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
		if publish {
			if err := component.Install(ctx); err != nil {
				return err
			}
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

// reconcileInstalledCarrier keeps the active carrier generation running, retires and removes every other carrier and
// hostd generation (sweepGenerations), then every pin nothing names.
// carrierReconcileBound bounds how long one reconcile pass waits for the active carrier while it holds the update lock.
const carrierReconcileBound = 5 * time.Second

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
	host, err = hostInstallerWithMissing(request, paths, true)
	if err != nil {
		return err
	}
	lifecycle, err := newLifecycleManagerWithCarrierScope(request, paths, host, hostd, updater, true)
	if err != nil {
		return err
	}
	if err := lifecycle.RequireRecovered(ctx); err != nil {
		return hostInstallFailure("native installation recovery is required before carrier reconciliation", err)
	}
	active, err := ReadCarrierConfig(ctx, instance)
	if err != nil {
		return err
	}
	// The update lock is held for this pass: a carrier that does not come up must not hold it for long.
	ensureCtx, cancel := context.WithTimeout(ctx, carrierReconcileBound)
	defer cancel()
	if err := EnsureExistingCarrier(ensureCtx, active); err != nil {
		return err
	}
	err = sweepGenerations(ctx, request, active.Generation, false)
	// Pins go last in the same pass: a generation's pin outlives it until nothing names it any more (pbvm1 F14-04:
	// the replaced build's pin stayed after its drain, once the install's own inventory had run).
	catalog, _, catalogErr := sessionOwnerCatalog(request)
	if catalogErr != nil {
		return errors.Join(err, catalogErr)
	}
	return errors.Join(err, pruneSessionOwnerPinsAt(ctx, catalog, paths, request.UID, request.StateRoot, request.Source.SHA256))
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
	return []*service.NativeTransactionalComponent{component}, nil
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
	host, err = hostInstallerWithMissing(installed, paths, true)
	if err != nil {
		return err
	}
	// Terminal recovery must not wait for the independent power-policy service.
	for _, installer := range []*service.Installer{hostd, host, updater} {
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
