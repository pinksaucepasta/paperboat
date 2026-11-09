//go:build windows

package hostinstall

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"golang.org/x/sys/windows"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/workerupdate"
	"github.com/pinksaucepasta/paperboat/internal/windowsopenssh"
)

// windowsInstallationState restores the protected native baseline, not a
// caller-supplied official marker. Its source is the existing installed record.
type windowsInstallationState struct {
	layout              service.Layout
	source              *installsource.Source
	offline             bool
	preparedCandidateID string
}
type windowsInstallationSnapshot struct {
	PreparedCandidateID string                          `json:"prepared_candidate_id,omitempty"`
	Offline             bool                            `json:"offline,omitempty"`
	InstallationAbsent  bool                            `json:"installation_absent"`
	Config              *WindowsRuntimeConfig           `json:"config"`
	Source              *installsource.Source           `json:"source"`
	Carrier             *CarrierConfig                  `json:"carrier"`
	LegacySSH           *windowsopenssh.ServiceSnapshot `json:"legacy_ssh"`
}

func WindowsInstallationState(ownerSID string, source *installsource.Source) (service.NativeInstallationState, error) {
	layout, err := service.WindowsUserLayout(ownerSID)
	if err != nil {
		return nil, err
	}
	return windowsInstallationState{layout: layout, source: source}, nil
}

// WindowsPreparedInstallationState binds native recovery to the exact approved
// candidate whose signed helper owns the deployment gate transaction.
func WindowsPreparedInstallationState(ownerSID string, source *installsource.Source, candidateID string) (service.NativeInstallationState, error) {
	if !validWindowsPreparedCandidateID(candidateID) || source == nil || source.Validate() != nil {
		return nil, ErrInvalidRequest
	}
	layout, err := service.WindowsUserLayout(ownerSID)
	if err != nil {
		return nil, err
	}
	return windowsInstallationState{layout: layout, source: source, preparedCandidateID: candidateID}, nil
}

func validWindowsPreparedCandidateID(candidateID string) bool {
	decoded, err := hex.DecodeString(candidateID)
	return err == nil && len(decoded) == 32 && len(candidateID) == 64 && strings.ToLower(candidateID) == candidateID
}

// WindowsInstallationStateForRecovery records an explicitly requested offline
// bundle's retained signed proof. Offline mode is never inferred from failure.
func WindowsInstallationStateForRecovery(ownerSID string, source *installsource.Source, offline bool) (service.NativeInstallationState, error) {
	layout, err := service.WindowsUserLayout(ownerSID)
	if err != nil {
		return nil, err
	}
	return windowsInstallationState{layout: layout, source: source, offline: offline}, nil
}

func (s windowsInstallationState) Capture(ctx context.Context) ([]byte, error) {
	carrier, err := readCarrierConfigFile(ctx, s.layout.Instance)
	var retained *CarrierConfig
	if err == nil {
		if err := carrier.Source.Verify(carrier.Executable); err != nil {
			return nil, err
		}
		retained = &carrier
	} else if !installNotExistOnly(err) {
		return nil, err
	}
	config, err := LoadWindowsRuntimeConfigForInstance(s.layout.Instance)
	if installNotExistOnly(err) {
		if retained != nil {
			return nil, ErrInvalidRequest
		}
		return json.Marshal(windowsInstallationSnapshot{InstallationAbsent: true})
	}
	if err != nil {
		return nil, err
	}
	source := config.Source
	if s.source != nil {
		source = *s.source
	}
	pin, err := WindowsPinnedSourcePath(s.layout, source)
	if err != nil {
		return nil, err
	}
	if err := verifyWindowsSource(ctx, pin, source, config.OwnerSID); err != nil {
		return nil, err
	}
	var ssh *windowsopenssh.ServiceSnapshot
	if retained == nil {
		sshConfig, err := windowsOpenSSHConfig(s.layout, config.OwnerSID)
		if err != nil {
			return nil, err
		}
		ssh, err = windowsopenssh.CaptureService(ctx, sshConfig)
		if err != nil {
			return nil, err
		}
		if ssh.Present {
			if err := verifyWindowsSource(ctx, sshConfig.ServiceExecutable, source, config.OwnerSID); err != nil {
				return nil, err
			}
		}

	}
	return json.Marshal(windowsInstallationSnapshot{Config: &config, Source: &source, Carrier: retained, LegacySSH: ssh, Offline: s.offline, PreparedCandidateID: s.preparedCandidateID})
}

// AuthorizeRecovery is called by the owning lifecycle under its lock,
// including committed-journal retirement, before restoring or removing anything.
func (s windowsInstallationState) AuthorizeRecovery(ctx context.Context, snapshot []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(snapshot) == 0 {
		return nil
	}
	state, err := decodeWindowsInstallationSnapshot(snapshot)
	if err != nil {
		return err
	}
	if err := s.authorizePreparedSnapshot(state); err != nil {
		return err
	}
	if state.Offline != s.offline || !validWindowsInstance(s.layout.Instance) || (state.Config == nil) != (state.Source == nil) || state.InstallationAbsent != (state.Config == nil) {
		return ErrInvalidRequest
	}
	if state.Config == nil {
		if state.Carrier != nil || s.source != nil || state.PreparedCandidateID != "" {
			return ErrInvalidRequest
		}
		return nil
	}
	config := *state.Config
	if !validWindowsConfig(config) || config.Instance != s.layout.Instance || state.Source.Validate() != nil || *state.Source != config.Source {
		return ErrInvalidRequest
	}
	instance, err := WindowsInstanceForSID(config.OwnerSID)
	if err != nil || instance != s.layout.Instance || s.source != nil && *s.source != *state.Source {
		return ErrInvalidRequest
	}
	if state.Carrier != nil && (state.Carrier.Instance != s.layout.Instance || state.Carrier.OwnerSID != config.OwnerSID || state.Carrier.Validate() != nil) {
		return ErrInvalidRequest
	}
	return nil
}

func decodeWindowsInstallationSnapshot(snapshot []byte) (windowsInstallationSnapshot, error) {
	var state windowsInstallationSnapshot
	if len(snapshot) == 0 || len(snapshot) > 128<<10 {
		return state, ErrInvalidRequest
	}
	decoder := json.NewDecoder(bytes.NewReader(snapshot))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&state) != nil || decoder.Decode(&struct{}{}) != io.EOF {
		return state, ErrInvalidRequest
	}
	return state, nil
}

func (s windowsInstallationState) authorizePreparedSnapshot(state windowsInstallationSnapshot) error {
	if state.PreparedCandidateID != "" && !validWindowsPreparedCandidateID(state.PreparedCandidateID) {
		return ErrInvalidRequest
	}
	if state.PreparedCandidateID != s.preparedCandidateID {
		if state.PreparedCandidateID != "" {
			return ErrSignedUpdatePending
		}
		return ErrInvalidRequest
	}
	return nil
}

func (s windowsInstallationState) Restore(ctx context.Context, snapshot []byte) error {
	if err := s.AuthorizeRecovery(ctx, snapshot); err != nil {
		return err
	}
	state, err := decodeWindowsInstallationSnapshot(snapshot)
	if err != nil {
		return err
	}
	if err := s.authorizePreparedSnapshot(state); err != nil {
		return err
	}
	if !validWindowsInstance(s.layout.Instance) || (state.Config == nil) != (state.Source == nil) || state.InstallationAbsent != (state.Config == nil) {
		return ErrInvalidRequest
	}
	if state.Carrier != nil && (state.Carrier.Instance != s.layout.Instance || state.Carrier.Validate() != nil) {
		return ErrInvalidRequest
	}
	if state.Config == nil {
		if state.Carrier != nil || s.source != nil {
			return ErrInvalidRequest
		}
		if err := restoreWindowsCarrierSnapshot(ctx, s.layout, nil); err != nil {
			return err
		}
		if err := stopWindowsRuntimeServices(ctx, s.layout.Instance); err != nil {
			return err
		}
		root, _ := WindowsInstanceRoot(s.layout.Instance)
		path := filepath.Join(root, "runtime-install.json")
		// Remove only the protected installation record created by this task.
		if config, err := LoadWindowsRuntimeConfigForInstance(s.layout.Instance); err == nil {
			_ = config
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		} else if !installNotExistOnly(err) {
			return err
		}
		return nil
	}
	config := *state.Config
	if !validWindowsConfig(config) || config.Instance != s.layout.Instance || state.Source.Validate() != nil || state.Source.Platform != "windows" || state.Source.Architecture != config.Source.Architecture {
		return ErrInvalidRequest
	}
	if s.source != nil && *state.Source != *s.source {
		return ErrInvalidRequest
	}
	expected, err := WindowsInstanceForSID(config.OwnerSID)
	if err != nil || expected != s.layout.Instance {
		return ErrInvalidRequest
	}
	pin, err := WindowsPinnedSourcePath(s.layout, *state.Source)
	if err != nil {
		return err
	}
	if err := verifyWindowsSource(ctx, pin, *state.Source, config.OwnerSID); err != nil {
		return err
	}
	policy := workerupdate.TUFSource{RepositoryURL: config.Artifact.RepositoryURL, StateRoot: filepath.Join(s.layout.UpdateStateRoot, "tuf")}
	authorize := func(source installsource.Source, executable string) error {
		if err := verifyWindowsSource(ctx, executable, source, config.OwnerSID); err != nil {
			return err
		}
		if source.Distribution != installsource.Official {
			return nil
		}
		if state.Offline {
			return policy.AuthorizeOfflineRecovery(ctx, source.Version, "windows", source.Architecture)
		}
		return policy.AuthorizeRecovery(ctx, source.Version, "windows", source.Architecture)
	}
	if err := authorize(*state.Source, pin); err != nil {
		return hostInstallFailure("authorize signed Windows installation recovery", err)
	}
	if state.Carrier != nil {
		if err := authorize(state.Carrier.Source, state.Carrier.Executable); err != nil {
			return hostInstallFailure("authorize signed Windows carrier recovery", err)
		}
	}
	if err := restoreWindowsCarrierSnapshot(ctx, s.layout, state.Carrier); err != nil {
		return err
	}
	restoreSSH := func() error {
		if state.LegacySSH != nil {
			sshConfig, err := windowsOpenSSHConfig(s.layout, config.OwnerSID)
			if err != nil {
				return err
			}
			if state.LegacySSH.Present {
				args, err := windows.DecomposeCommandLine(state.LegacySSH.Configuration.BinaryPathName)
				if err != nil || len(args) == 0 {
					return ErrInvalidRequest
				}
				sshConfig.ServiceExecutable = args[0]
				versionPin := filepath.Join(s.layout.ReleasesRoot, "versions", state.Source.Version, "pb.exe")
				if !strings.EqualFold(args[0], s.layout.Binary) && !strings.EqualFold(args[0], pin) && !strings.EqualFold(args[0], versionPin) {
					return ErrInvalidRequest
				}
				if err := verifyWindowsSource(ctx, args[0], *state.Source, config.OwnerSID); err != nil {
					return err
				}
			}
			if err := windowsopenssh.RestoreService(ctx, sshConfig, *state.LegacySSH); err != nil {
				return err
			}
		}

		return nil
	}
	// Carrier-only migration leaves the installed runtime identity unchanged.
	// Its rollback runs inside Updated and must never stop its own service.
	installed, loadErr := LoadWindowsRuntimeConfigForInstance(s.layout.Instance)
	if loadErr != nil && !installNotExistOnly(loadErr) {
		return loadErr
	}
	if loadErr == nil {
		have, _ := json.Marshal(installed)
		want, _ := json.Marshal(config)
		if string(have) == string(want) && installed.Source == *state.Source {
			if err := verifyWindowsSource(ctx, s.layout.Binary, *state.Source, config.OwnerSID); err == nil {
				return restoreSSH()
			}
			// Replacement promotes bytes before metadata. An interrupted
			// promotion with unchanged metadata still needs baseline restoration.
		}
	}
	if err := stopWindowsRuntimeServices(ctx, s.layout.Instance); err != nil {
		return err
	}
	if err := terminateStaleWindowsRuntimeProcesses(ctx, s.layout.Binary); err != nil {
		return err
	}
	current, rollback, _ := windowsRuntimePaths(s.layout)
	if err := stageWindowsBinary(ctx, pin, current, rollback, *state.Source, config.OwnerSID, nil); err != nil {
		return err
	}
	root, err := WindowsInstanceRoot(s.layout.Instance)
	if err != nil {
		return err
	}
	if err := writeWindowsConfigAt(config, root); err != nil {
		return err
	}
	if err := restoreSSH(); err != nil {
		return err
	}
	return restoreWindowsSessionOwner(ctx, config, s.layout)
}

var prepareRestoredSessionOwner = PrepareSessionOwner

// restoreWindowsSessionOwner starts the restored installation's session owner again, as repair does, before the
// lifecycle restores its services: hostd exits without its owner's pipe (victus 2026-11-10: the rolled-back hostd
// found none and stayed stopped).
func restoreWindowsSessionOwner(ctx context.Context, config WindowsRuntimeConfig, layout service.Layout) error {
	if config.SessionOwnerGeneration == "" {
		return nil
	}
	request := windowsRepairRequest(config)
	request.Executable, request.WorkspaceRoot = layout.Binary, config.Workspace
	if err := prepareRestoredSessionOwner(ctx, request); err != nil {
		return hostInstallFailure("start the previous version's session owner", err)
	}
	return nil
}

// windowsHostdRunning reports whether the installed hostd service runs.
var windowsHostdRunning = func(ctx context.Context, layout service.Layout) (bool, error) {
	hostd, _, _, err := windowsRoleInstallers(layout, true)
	if err != nil {
		return false, err
	}
	status, err := (service.WindowsController{}).Inspect(ctx, hostd.DefinitionPath())
	return status.Running, err
}

// prepareWindowsReplacementMaintenance asks the running hostd to admit maintenance. A stopped hostd holds no work
// to protect and has no pipe to ask, so installing over a broken installation proceeds (the rule repair follows).
func prepareWindowsReplacementMaintenance(ctx context.Context, layout service.Layout, tokenFile string, force bool, timeout time.Duration) (func() error, error) {
	running, err := windowsHostdRunning(ctx, layout)
	if err != nil {
		return nil, err
	}
	if !running {
		return func() error { return nil }, nil
	}
	return prepareMaintenance(ctx, layout.HostdSocket, tokenFile, force, timeout)
}

// PrepareWindowsSuppliedMaintenance is prepareWindowsReplacementMaintenance for the supplied-install command.
func PrepareWindowsSuppliedMaintenance(ctx context.Context, layout service.Layout, tokenFile string, force bool) (func(), error) {
	abort, err := prepareWindowsReplacementMaintenance(ctx, layout, tokenFile, force, 35*time.Second)
	if err != nil {
		return nil, err
	}
	return func() { _ = abort() }, nil
}

// replaceWindowsCandidate is the one way a Windows installation is replaced by request's source: a failed attempt
// leaves the previous version complete (lifecycle rollback, machine guard) and nothing of the candidate.
func replaceWindowsCandidate(ctx context.Context, manager *service.LifecycleManager, request Request, replace func(context.Context) error) error {
	candidate, err := newCandidateSessionOwner(request)
	if err != nil {
		return err
	}
	installGuard := func(ctx context.Context) (guardUndo, error) {
		var undo guardUndo
		err := runWindowsInstallPhase(ctx, "install Paperboat machine guard", func() (err error) {
			undo, err = installMachineGuardReversible(ctx, request)
			return err
		})
		if err != nil {
			return nil, err
		}
		return undo, nil
	}
	return replaceCandidate(ctx, manager.Replace, installGuard, replace, func(ctx context.Context) error {
		// A rollback that did not finish keeps its journal: recovery still owns both versions.
		if manager.RequireRecovered(ctx) != nil {
			return nil
		}
		return withdrawWindowsCandidate(ctx, request, candidate)
	})
}

// withdrawWindowsCandidate removes what a replacement that did not commit published for its source: the session
// owner (service and catalogue descriptor) and the SSH generation (victus 2026-11-10: both stayed after the
// rollback). The restored installation's own generation is never touched.
func withdrawWindowsCandidate(ctx context.Context, request Request, candidate candidateSessionOwner) error {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	instance, err := WindowsInstanceForSID(request.OwnerSID)
	if err != nil {
		return err
	}
	// A first installation that failed leaves no record: nothing is committed.
	installed, err := LoadWindowsRuntimeConfigForInstance(instance)
	if err != nil && !installNotExistOnly(err) {
		return err
	}
	return hostInstallFailure("remove the failed version's session owner and SSH service",
		candidate.withdrawUnless(ctx, installed.SessionOwnerGeneration),
		withdrawWindowsSSHCandidate(ctx, instance, request, installed.Source))
}

func withdrawWindowsSSHCandidate(ctx context.Context, instance string, request Request, committed installsource.Source) error {
	if committed.SHA256 == request.Source.SHA256 {
		return nil
	}
	generations, err := windowsopenssh.ListGenerations(ctx, request.OwnerSID)
	if err != nil {
		return err
	}
	for _, generation := range generations {
		if generation.Source.SHA256 != request.Source.SHA256 {
			continue
		}
		present, err := windowsCarrierPresentForSSH(ctx, instance)
		if err != nil {
			return err
		}
		if !present {
			// No carrier is left that could have admitted a connection to it.
			return windowsopenssh.RetireGeneration(ctx, generation)
		}
		// The restored carrier knows which generations still hold connections; a busy one is retired by the
		// updater's reconciliation once they end.
		carrier, err := ReadCarrierConfig(ctx, instance)
		if err != nil {
			return err
		}
		return pruneWindowsSSHGenerations(ctx, carrier, committed)
	}
	return nil
}

// Preparation can probe/stop legacy roles before lifecycle recovery. Reject a
// foreign signed owner before that preparation; Recover repeats the check under
// its own lock so this read-only preflight cannot grant mutation authority.
func requireWindowsNativeRecoveryOwner(ctx context.Context, layout service.Layout) error {
	state := windowsInstallationState{layout: layout}
	manager, err := newWindowsLifecycleManager(Request{Platform: "windows"}, layout, true)
	if err != nil {
		return err
	}
	snapshot, err := manager.CapturedInstallation(ctx)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return state.AuthorizeRecovery(ctx, snapshot)
}

// RecoverWindowsInstallation restores a crash-interrupted native replacement
// before admission relies on an old hostd endpoint.
func RecoverWindowsInstallation(ctx context.Context, ownerSID string, state service.NativeInstallationState) (resultErr error) {
	unlock, err := LockWindowsNativePublication(ctx)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, unlock()) }()
	return recoverWindowsInstallationLocked(ctx, ownerSID, state)
}

func recoverWindowsInstallationLocked(ctx context.Context, ownerSID string, state service.NativeInstallationState) error {
	layout, err := service.WindowsUserLayout(ownerSID)
	if err != nil {
		return err
	}
	// Recovery must probe dependency readiness before starting Updated. Its
	// startup may otherwise attempt LocalDaemon migration and re-enter this
	// lifecycle journal while recovery still holds the journal lock.
	config, err := LoadWindowsRuntimeConfigForInstance(layout.Instance)
	if err != nil {
		return err
	}
	request := windowsRepairRequest(config)
	manager, err := newWindowsLifecycleManager(request, layout, true)
	if err != nil {
		return err
	}
	if state != nil {
		manager, err = newWindowsLifecycleWithState(request, layout, state)
		if err != nil {
			return err
		}
	}
	return manager.Recover(ctx)
}

// PrepareWindowsReplacement pins both the protected prior image and candidate
// before any stop. Only a newly created, unused candidate is removed on abort.
func PrepareWindowsReplacement(ctx context.Context, request Request) (func(), error) {
	// A first install is pending enrollment and has no machine identity; the
	// source verification below still binds the exact candidate.
	if !request.EnrollmentPending {
		if err := Validate(request, 0); err != nil {
			return nil, err
		}
	}
	if err := request.Source.Verify(request.Executable); err != nil {
		return nil, err
	}
	layout, err := service.WindowsUserLayout(request.OwnerSID)
	if err != nil {
		return nil, err
	}
	if err := preflightWindowsCarrierCutover(ctx, layout.Instance, request.OwnerSID, request.StateRoot); err != nil {
		return nil, err
	}
	if previous, err := LoadWindowsRuntimeConfigForInstance(layout.Instance); err == nil {
		prior := previous.Source
		if request.RollbackIdentity != nil {
			prior = request.RollbackIdentity.Source
		}
		if err := prior.Verify(layout.Binary); err != nil {
			return nil, err
		}
		if _, err := ensureWindowsPinnedRuntimeFrom(ctx, layout, prior, request.OwnerSID, layout.Binary); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	pin, err := WindowsPinnedSourcePath(layout, request.Source)
	if err != nil {
		return nil, err
	}
	_, statErr := os.Lstat(pin)
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return nil, statErr
	}
	if _, err := ensureWindowsPinnedRuntimeFrom(ctx, layout, request.Source, request.OwnerSID, request.Executable); err != nil {
		return nil, err
	}
	return func() {
		if statErr == nil {
			return
		}
		if installed, err := LoadWindowsRuntimeConfigForInstance(layout.Instance); err == nil && installed.Source == request.Source {
			return
		}
		// A session owner or SSH generation of the candidate that could not be
		// withdrawn (a rollback that did not finish, connections its carrier still
		// holds) runs this pin. The normal reference-aware GC owns its retirement.
		catalogue, _, err := sessionOwnerCatalog(request)
		if err != nil {
			return
		}
		descriptors, err := catalogue.ReadAll(context.WithoutCancel(ctx), layout.Instance)
		if err != nil {
			return
		}
		for _, descriptor := range descriptors {
			if descriptor.Source.SameArtifact(request.Source) {
				return
			}
		}
		generations, err := windowsopenssh.ListGenerations(context.WithoutCancel(ctx), request.OwnerSID)
		if err != nil {
			return
		}
		for _, generation := range generations {
			if generation.Source.SHA256 == request.Source.SHA256 {
				return
			}
		}
		if request.Source.Verify(pin) == nil {
			_ = os.Remove(pin)
			_ = os.Remove(filepath.Dir(pin))
		}
	}, nil
}

// ReplaceWindowsInstallation uses the existing native lifecycle journal for
// supplied replacement bytes/config and the exact prior SCM definitions.
func ReplaceWindowsInstallation(ctx context.Context, request Request, state service.NativeInstallationState, replace func(context.Context) error) error {
	return replaceWindowsInstallation(ctx, request, state, replace, PreparedUpdateHooks{})
}

func replaceWindowsInstallation(ctx context.Context, request Request, state service.NativeInstallationState, replace func(context.Context) error, hooks PreparedUpdateHooks) (resultErr error) {
	if err := Validate(request, 0); err != nil {
		return err
	}
	if state == nil || replace == nil {
		return ErrInvalidRequest
	}
	if err := request.Source.Verify(request.Executable); err != nil {
		return err
	}
	unlock, err := LockWindowsNativePublication(ctx)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, unlock()) }()
	return replaceWindowsInstallationLocked(ctx, request, state, replace, hooks)
}

func replaceWindowsInstallationLocked(ctx context.Context, request Request, state service.NativeInstallationState, replace func(context.Context) error, hooks PreparedUpdateHooks) error {
	layout, err := service.WindowsUserLayout(request.OwnerSID)
	if err != nil {
		return err
	}
	manager, err := newWindowsLifecycleWithState(request, layout, state)
	if err != nil {
		return err
	}
	if err := manager.Recover(ctx); err != nil {
		return err
	}
	installed, err := LoadWindowsRuntimeConfigForInstance(layout.Instance)
	if err != nil {
		return err
	}
	if installed.OwnerSID != request.OwnerSID || installed.StateRoot != request.StateRoot {
		return ErrInvalidRequest
	}
	if hooks.Prepare != nil && (request.RollbackIdentity == nil || !installed.Committed || installed.Source != request.RollbackIdentity.Source || installed.MachineID != request.UserMachineID || installed.ControlURL != request.ControlURL || installed.Workspace != request.WorkspaceRoot || installed.ListenAddress != request.HelperListenAddress) {
		return ErrInvalidRequest
	}
	if err := preflightWindowsCarrierCutover(ctx, layout.Instance, installed.OwnerSID, installed.StateRoot); err != nil {
		return err
	}
	present, err := windowsCarrierPresentForSSH(ctx, layout.Instance)
	if err != nil {
		return err
	}
	if present {
		timeout := hooks.DrainTimeout
		if timeout == 0 {
			timeout = 35 * time.Second
		}
		abort, err := prepareWindowsReplacementMaintenance(ctx, layout, installed.TokenFile, request.Force, timeout)
		if err != nil {
			return err
		}
		defer abort()
	}
	if hooks.Prepare != nil {
		if err := hooks.Prepare(ctx); err != nil {
			return err
		}
	}
	cleanup, err := PrepareWindowsReplacement(ctx, request)
	if err != nil {
		return err
	}
	defer cleanup()
	return replaceWindowsCandidate(ctx, manager, request, replace)
}

// InstallWindowsReplacement is called only inside the owning lifecycle Replace.
func InstallWindowsReplacement(ctx context.Context, request Request) error {
	return installWindows(ctx, request, true)
}

func StopWindowsReplacement(ctx context.Context, ownerSID string) error {
	instance, err := WindowsInstanceForSID(ownerSID)
	if err != nil {
		return err
	}
	return stopWindowsRuntimeServices(ctx, instance)
}

// CapturedWindowsSource projects a protected native baseline while candidate
// runtime configuration is uncommitted; it never performs lifecycle recovery.
func CapturedWindowsSource(ctx context.Context, ownerSID string) (installsource.Source, error) {
	return capturedWindowsSource(ctx, ownerSID, "")
}

// CapturedWindowsPreparedSource requires native mutation proof belonging to the
// immutable approved candidate, rather than a staging phase or matching baseline.
func CapturedWindowsPreparedSource(ctx context.Context, ownerSID, candidateID string) (installsource.Source, error) {
	if !validWindowsPreparedCandidateID(candidateID) {
		return installsource.Source{}, ErrInvalidRequest
	}
	return capturedWindowsSource(ctx, ownerSID, candidateID)
}

// windowsCapturedSourceProjection reads the source out of a captured installation snapshot. A snapshot that
// recorded no installation (first enrollment) has no predecessor: os.ErrNotExist, which callers read as
// "nothing captured". Anything else that lacks a valid source is a corrupt record.
func windowsCapturedSourceProjection(body []byte, candidateID string) (*installsource.Source, error) {
	var projection struct {
		Source              *installsource.Source
		InstallationAbsent  bool   `json:"installation_absent"`
		PreparedCandidateID string `json:"prepared_candidate_id"`
		Installation        []byte `json:"installation"`
	}
	if err := json.Unmarshal(body, &projection); err != nil {
		return nil, ErrInvalidRequest
	}
	if candidateID != "" && (projection.PreparedCandidateID != candidateID || len(projection.Installation) != 0) {
		return nil, ErrSignedUpdatePending
	}
	if len(projection.Installation) != 0 {
		if err := json.Unmarshal(projection.Installation, &projection); err != nil {
			return nil, ErrInvalidRequest
		}
	}
	if projection.InstallationAbsent && projection.Source == nil {
		return nil, os.ErrNotExist
	}
	if projection.Source == nil || projection.Source.Validate() != nil {
		return nil, ErrInvalidRequest
	}
	return projection.Source, nil
}

func capturedWindowsSource(ctx context.Context, ownerSID, candidateID string) (installsource.Source, error) {
	layout, err := service.WindowsUserLayout(ownerSID)
	if err != nil {
		return installsource.Source{}, err
	}
	config, err := LoadWindowsRuntimeConfigForInstance(layout.Instance)
	if err != nil {
		return installsource.Source{}, err
	}
	manager, err := newWindowsLifecycleWithState(windowsRepairRequest(config), layout, windowsInstallationState{layout: layout})
	if err != nil {
		return installsource.Source{}, err
	}
	body, err := manager.CapturedInstallation(ctx)
	if err != nil {
		return installsource.Source{}, err
	}
	source, err := windowsCapturedSourceProjection(body, candidateID)
	if err != nil {
		return installsource.Source{}, err
	}
	pin, err := WindowsPinnedSourcePath(layout, *source)
	if err != nil {
		return installsource.Source{}, err
	}
	if err := verifyWindowsSource(ctx, pin, *source, ownerSID); err != nil {
		return installsource.Source{}, err
	}
	return *source, nil
}
