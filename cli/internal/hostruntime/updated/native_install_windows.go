//go:build windows

package updated

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostinstall"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/workerupdate"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

var ErrNativeInstalledSourceMismatch = errors.New("installed native runtime does not match its protected source")

// PrepareWindowsNativeInstall is called with the instance installation mutex
// held, after synchronously stopping the updater. It retires only completed
// transactions; the returned callback restores their policy state on failure,
// before the prior services restart. Signed metadata and release slots remain.
func PrepareWindowsNativeInstall(ctx context.Context, ownerSID string) (*hostinstall.WindowsRollbackIdentity, func() error, error) {
	return prepareWindowsNativeInstall(ctx, ownerSID, true, false)
}

func prepareWindowsNativeInstall(ctx context.Context, ownerSID string, retire, offline bool) (identity *hostinstall.WindowsRollbackIdentity, restore func() error, resultErr error) {
	noop := func() error { return nil }
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	layout, err := service.WindowsUserLayout(ownerSID)
	if err != nil {
		return nil, nil, err
	}
	config, err := hostinstall.LoadWindowsRuntimeConfigForInstance(layout.Instance)
	if errors.Is(err, os.ErrNotExist) {
		return nil, noop, nil
	}
	if err != nil {
		return nil, nil, err
	}
	manager, err := mgr.Connect()
	if err != nil {
		return nil, nil, err
	}
	defer func() { resultErr = errors.Join(resultErr, manager.Disconnect()) }()
	updater, err := manager.OpenService(windowsUpdaterService + "-" + layout.Instance)
	if err == nil {
		status, queryErr := updater.Query()
		closeErr := updater.Close()
		if err := errors.Join(queryErr, closeErr); err != nil {
			return nil, nil, err
		}
		if retire && status.State != svc.Stopped {
			return nil, nil, fmt.Errorf("stop the updater before reinstalling: %w", ErrWindowsActivationUnavailable)
		}
	} else if !errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		return nil, nil, err
	}
	activator, err := manager.OpenService(windowsActivatorService + "-" + layout.Instance)
	if err == nil {
		closeErr := activator.Close()
		return nil, nil, errors.Join(fmt.Errorf("finish or recover the pending update before reinstalling: %w", ErrWindowsActivationUnavailable), closeErr)
	}
	if !errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		return nil, nil, err
	}
	updateConfig := WindowsConfig{OwnerSID: ownerSID, StateRoot: layout.UpdateStateRoot}
	journal, err := loadWindowsActivationJournal(updateConfig)
	if errors.Is(err, os.ErrNotExist) {
		if err := config.Source.Verify(layout.Binary); err == nil {
			signed := config.Source.Distribution == installsource.Official
			if signed {
				policy := workerupdate.TUFSource{RepositoryURL: config.Artifact.RepositoryURL, StateRoot: filepath.Join(layout.UpdateStateRoot, "tuf")}
				if err := authorizeWindowsNativeRecovery(ctx, policy, config.Source, offline); err != nil {
					return nil, nil, err
				}
			}
			return &hostinstall.WindowsRollbackIdentity{Source: config.Source, Signed: signed}, noop, nil
		}
		// Native repair can select the previously authenticated signed slot.
		// Its durable rollback identity remains the proof after journal retirement.
		if !config.RollbackSigned || config.RollbackSource == nil {
			return nil, nil, ErrNativeInstalledSourceMismatch
		}
		identity := *config.RollbackSource
		if err := identity.Verify(layout.Binary); err != nil {
			return nil, nil, err
		}
		source := workerupdate.TUFSource{RepositoryURL: config.Artifact.RepositoryURL, StateRoot: filepath.Join(layout.UpdateStateRoot, "tuf")}
		if err := authorizeWindowsNativeRecovery(ctx, source, identity, offline); err != nil {
			return nil, nil, err
		}
		return &hostinstall.WindowsRollbackIdentity{Source: identity, Signed: true}, noop, nil
	}
	if err != nil {
		return nil, nil, err
	}
	if !windowsMachineFileSecurityMatches(windowsActivationJournalPath(layout.UpdateStateRoot), "D:P(A;;FA;;;SY)(A;;FA;;;BA)") {
		return nil, nil, errInvalidWindowsActivation
	}
	var sourceIdentity = config.Source
	var signed bool
	if windowsNativeInstallMaySupersede(journal) {
		// This download never changed the installation. Preserve its exact journal
		// for rollback instead of approving or launching it during explicit install.
		if err := validateWindowsNativeInstallPreviousSource(config.Source, journal); err != nil {
			return nil, nil, err
		}
		if err := verifyWindowsPreparedCandidate(ctx, journal); err != nil {
			return nil, nil, err
		}
	} else {
		if !nativeWindowsJournalRetirable(journal) {
			return nil, nil, fmt.Errorf("finish or recover the pending update before reinstalling: %w", ErrWindowsActivationUnavailable)
		}
		sourceIdentity, signed, err = nativeWindowsRollbackSource(journal)
		if err != nil {
			return nil, nil, err
		}
	}
	sourceIdentity.AutomaticUpdates = config.Source.AutomaticUpdates
	if err := sourceIdentity.Verify(layout.Binary); err != nil {
		return nil, nil, err
	}
	target := workerupdate.ComponentTarget{SHA256: sourceIdentity.SHA256, Length: sourceIdentity.Length, Platform: "windows", Architecture: sourceIdentity.Architecture}
	if err := verifyWindowsStableBinary(ctx, layout.Binary, target, ownerSID); err != nil {
		return nil, nil, err
	}
	if signed {
		source := workerupdate.TUFSource{RepositoryURL: config.Artifact.RepositoryURL, StateRoot: filepath.Join(layout.UpdateStateRoot, "tuf")}
		if err := authorizeWindowsNativeRecovery(ctx, source, sourceIdentity, offline); err != nil {
			return nil, nil, err
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if retire {
		if err := os.Remove(windowsActivationJournalPath(layout.UpdateStateRoot)); err != nil {
			return nil, nil, err
		}
	}
	return &hostinstall.WindowsRollbackIdentity{Source: sourceIdentity, Signed: signed}, func() error { return (&windowsSCMActivationBackend{config: updateConfig}).WriteJournal(journal) }, nil
}

// RecoverWindowsNativeInstall runs under the installer's existing instance
// mutex, before any owner services or installed bytes are replaced. It resumes
// an already-approved transaction through the existing signed activator.
func RecoverWindowsNativeInstall(ctx context.Context, config WindowsConfig) error {
	ops := windowsNativeInstallRecoveryOps{
		load: func() (windowsActivationJournal, error) {
			if err := secureWindowsFileShape(windowsActivationJournalPath(config.StateRoot)); err != nil {
				if errors.Is(err, os.ErrNotExist) {
					return windowsActivationJournal{}, os.ErrNotExist
				}
				// secureWindowsFileShape intentionally redacts OS errors; distinguish only
				// a genuinely absent journal before applying its protected-file checks.
				if _, statErr := os.Lstat(windowsActivationJournalPath(config.StateRoot)); errors.Is(statErr, os.ErrNotExist) {
					return windowsActivationJournal{}, os.ErrNotExist
				}
				return windowsActivationJournal{}, err
			}
			if !windowsMachineFileSecurityMatches(windowsActivationJournalPath(config.StateRoot), "D:P(A;;FA;;;SY)(A;;FA;;;BA)") {
				return windowsActivationJournal{}, errInvalidWindowsActivation
			}
			return loadWindowsActivationJournal(config)
		},
		validate: func(ctx context.Context, j windowsActivationJournal) error {
			if !validWindowsConfig(config) {
				return ErrInvalidWindowsConfig
			}
			if err := validateWindowsReadOnlyOwnerFile(config.TokenFile, config.OwnerSID); err != nil {
				return err
			}
			if err := validateWindowsReadOnlyOwnerFile(config.InstallState, config.OwnerSID); err != nil {
				return err
			}
			if err := validateWindowsPrivilegedInstallConfig(config); err != nil {
				return err
			}
			if err := validateWindowsNativeInstallJournalBinding(config, j); err != nil {
				return err
			}
			if windowsNativeInstallMaySupersede(j) {
				if err := validateWindowsNativeInstallPreviousSource(config.Source, j); err != nil {
					return err
				}
				layout, err := service.WindowsUserLayout(config.OwnerSID)
				if err != nil {
					return err
				}
				if err := config.Source.Verify(layout.Binary); err != nil {
					return err
				}
				target := workerupdate.ComponentTarget{SHA256: config.Source.SHA256, Length: config.Source.Length, Platform: "windows", Architecture: config.Source.Architecture}
				if err := verifyWindowsStableBinary(ctx, layout.Binary, target, config.OwnerSID); err != nil {
					return err
				}
			}
			if err := verifyWindowsPreparedCandidate(ctx, j); err != nil {
				return err
			}

			return nil
		},
		validateMutation: func(ctx context.Context, j windowsActivationJournal) error {

			return verifyWindowsNativeInstallRolePins(ctx, config, j)
		},
		owner: func(j windowsActivationJournal) (bool, bool, error) {
			return windowsNativeInstallActivatorState(config, j)
		},
		stopUpdater: func(ctx context.Context) error {
			_, name, err := windowsInstanceServiceNames(config.OwnerSID)
			if err != nil {
				return err
			}
			return stopNamedWindowsServices(ctx, name)
		},
		resume: func(ctx context.Context, j windowsActivationJournal) error {
			_, err := startVerifiedWindowsActivation(ctx, config, j)
			return err
		},
		validateTerminal: func(ctx context.Context, j windowsActivationJournal) error {
			return verifyWindowsTerminalNativeState(ctx, config, j)
		},
		finishTerminal: func(ctx context.Context, j windowsActivationJournal) error {

			// The installed activator can contain the bug that prevented retirement.
			// Finish its terminal transaction with the current verified installer,
			// without rerunning cutover or launching that older executable.
			backend := newWindowsSCMActivationBackend(config)
			result, activationErr := activateWindowsPreparedNative(ctx, backend, j)
			return finishWindowsActivatorResult(ctx, result, activationErr, backend.restoreFeatureUpdater, func() error {
				return retireWindowsActivatorService(config, result, true)
			})
		},
	}
	return recoverWindowsNativeInstall(ctx, ops)
}

func windowsNativeInstallActivatorState(config WindowsConfig, j windowsActivationJournal) (registered, running bool, resultErr error) {
	instance, _, _, _, name, err := windowsInstanceNames(config.OwnerSID)
	if err != nil {
		return false, false, err
	}
	manager, err := mgr.Connect()
	if err != nil {
		return false, false, err
	}
	defer func() { resultErr = errors.Join(resultErr, manager.Disconnect()) }()
	item, err := manager.OpenService(name)
	if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		return false, false, nil
	}
	if errors.Is(err, windows.ERROR_SERVICE_MARKED_FOR_DELETE) && nativeWindowsJournalRetirable(j) {
		return true, true, nil
	}
	if err != nil {
		return false, false, err
	}
	defer func() { resultErr = errors.Join(resultErr, item.Close()) }()
	definition, err := item.Config()
	if err != nil {
		return true, false, err
	}
	isRunning, err := windowsActivatorRunning(item, definition, instance, j)
	return true, isRunning, err
}

func windowsActivatorRunning(item *mgr.Service, definition mgr.Config, instance string, j windowsActivationJournal) (bool, error) {
	arguments, err := windows.DecomposeCommandLine(definition.BinaryPathName)
	installed, loadErr := hostinstall.LoadWindowsRuntimeConfigForInstance(instance)
	if loadErr != nil {
		return false, loadErr
	}
	helper, helperErr := windowsPreparedHelper(WindowsConfig{OwnerSID: installed.OwnerSID}, j)
	if helperErr != nil {
		return false, helperErr
	}
	expected := []string{helper, "daemon", "__runtime-activate", "--instance", instance}
	if err != nil || len(arguments) != len(expected) || !strings.EqualFold(arguments[0], expected[0]) || !slices.Equal(arguments[1:], expected[1:]) || !validPrivilegedWindowsServiceConfig(definition, mgr.StartAutomatic, mgr.ErrorSevere) {
		return false, errInvalidWindowsActivation
	}
	if err := validateWindowsRecovery(item); err != nil {
		return false, err
	}
	status, err := item.Query()
	if err != nil {
		return false, err
	}
	return status.State != svc.Stopped, nil
}

// Delete only the exact approved registered service. The installer requires a
// stopped owner; the activator itself uses the same check while it is running.
func retireWindowsActivatorService(config WindowsConfig, j windowsActivationJournal, requireStopped bool) (resultErr error) {
	instance, _, _, _, name, err := windowsInstanceNames(config.OwnerSID)
	if err != nil {
		return err
	}
	manager, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, manager.Disconnect()) }()
	item, err := manager.OpenService(name)
	if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) || errors.Is(err, windows.ERROR_SERVICE_MARKED_FOR_DELETE) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, item.Close()) }()
	definition, err := item.Config()
	if err != nil {
		return err
	}
	running, err := windowsActivatorRunning(item, definition, instance, j)
	if err != nil {
		return err
	}
	if requireStopped && running {
		return ErrWindowsActivationUnavailable
	}
	if err := item.Delete(); err != nil && !errors.Is(err, windows.ERROR_SERVICE_MARKED_FOR_DELETE) {
		return err
	}
	return nil
}

func verifyWindowsNativeInstallRolePins(ctx context.Context, config WindowsConfig, j windowsActivationJournal) error {
	if j.PreviousSource == nil {
		return errInvalidWindowsActivation
	}
	layout, err := service.WindowsUserLayout(config.OwnerSID)
	if err != nil {
		return err
	}
	candidate := installsource.Source{Version: j.Version, Platform: "windows", Architecture: j.Architecture, SHA256: j.Runtime.SHA256, Length: j.Runtime.Length, Distribution: installsource.Official, AutomaticUpdates: j.PreviousSource.AutomaticUpdates}
	sources := []installsource.Source{*j.PreviousSource, candidate}
	for _, role := range []struct{ name, kind, argument string }{{windowsHostdService + "-" + layout.Instance, service.HostdKind, "__runtime-hostd"}, {windowsUpdaterService + "-" + layout.Instance, service.UpdaterKind, "__runtime-updated"}} {
		actual, err := queryWindowsServiceTarget(role.name, role.argument)
		if err != nil {
			return err
		}
		identity, err := service.VerifyOwnedWindowsRoleExecutable(role.kind, layout.Instance, actual.Executable)
		if err != nil {
			return err
		}
		matched := false
		for _, source := range sources {
			pin, err := hostinstall.WindowsPinnedSourcePath(layout, source)
			if err != nil {
				return err
			}
			expected := windowsServiceTarget{Executable: pin, SHA256: source.SHA256, Length: source.Length, Arguments: []string{"daemon", role.argument, "--instance", layout.Instance}}
			if windowsNativeInstallPinMatches(expected, actual, identity) {
				matched = true
				break
			}
		}
		if !matched {
			return errInvalidWindowsActivation
		}
		target := workerupdate.ComponentTarget{SHA256: identity.SHA256, Length: identity.Length, Platform: "windows", Architecture: j.Architecture}
		if err := verifyWindowsPinnedBinary(ctx, actual.Executable, target, config.OwnerSID); err != nil {
			return err
		}
	}
	return nil
}

func windowsNativeInstallPinMatches(expected, actual windowsServiceTarget, identity service.WindowsExecutableIdentity) bool {
	return strings.EqualFold(expected.Executable, actual.Executable) && strings.EqualFold(identity.Executable, actual.Executable) && slices.Equal(expected.Arguments, actual.Arguments) && expected.SHA256 == identity.SHA256 && expected.Length == identity.Length && expected.Length > 0
}

func verifyWindowsTerminalNativeState(ctx context.Context, config WindowsConfig, j windowsActivationJournal) error {
	if !nativeWindowsJournalRetirable(j) || j.GateTarget == nil || j.PreviousSource == nil {
		return errInvalidWindowsActivation
	}
	layout, err := service.WindowsUserLayout(config.OwnerSID)
	if err != nil {
		return err
	}
	installed, err := hostinstall.LoadWindowsRuntimeConfigForInstance(layout.Instance)
	if err != nil {
		return err
	}
	if !installed.Committed || installed.OwnerSID != config.OwnerSID || installed.EnrollmentPending || j.GateTarget.MachineID != installed.MachineID {
		return errInvalidWindowsActivation
	}
	expected := *j.PreviousSource
	if j.Stage == windowsActivationCommitted {
		expected = installsource.Source{Version: j.Version, Platform: "windows", Architecture: j.Architecture, SHA256: j.Runtime.SHA256, Length: j.Runtime.Length, Distribution: installsource.Official, AutomaticUpdates: j.PreviousSource.AutomaticUpdates}
	}
	if installed.Source != expected {
		return errInvalidWindowsActivation
	}
	if _, err := hostinstall.CapturedWindowsSource(ctx, config.OwnerSID); !errors.Is(err, os.ErrNotExist) {
		return errors.Join(ErrWindowsActivationUnavailable, err)
	}
	if err := installed.Source.Verify(layout.Binary); err != nil {
		return err
	}
	if installed.Source.Distribution == installsource.Official {
		policy := workerupdate.TUFSource{RepositoryURL: installed.Artifact.RepositoryURL, StateRoot: filepath.Join(layout.UpdateStateRoot, "tuf")}
		if err := policy.AuthorizeRecovery(ctx, installed.Source.Version, "windows", installed.Source.Architecture); err != nil {
			return err
		}
	}
	pin, err := hostinstall.WindowsPinnedSourcePath(layout, installed.Source)
	if err != nil {
		return err
	}
	target := workerupdate.ComponentTarget{SHA256: installed.Source.SHA256, Length: installed.Source.Length, Platform: "windows", Architecture: installed.Source.Architecture}
	for _, role := range []struct{ name, argument string }{{windowsHostdService + "-" + layout.Instance, "__runtime-hostd"}, {windowsUpdaterService + "-" + layout.Instance, "__runtime-updated"}, {"PaperboatLocalDaemon-" + layout.Instance, "__runtime-local-daemon"}} {
		actual, err := queryWindowsServiceTarget(role.name, role.argument)
		if err != nil {
			return err
		}
		if !strings.EqualFold(actual.Executable, pin) || !slices.Equal(actual.Arguments, []string{"daemon", role.argument, "--instance", layout.Instance}) {
			return errInvalidWindowsActivation
		}
		if err := verifyWindowsPinnedBinary(ctx, actual.Executable, target, config.OwnerSID); err != nil {
			return err
		}
		declaration := filepath.Join(hostinstall.WindowsProgramDataRoot(), "services", role.name+".json")
		status, err := (service.WindowsController{}).Inspect(ctx, declaration)
		if err != nil || !status.Registered || !status.Enabled || !status.Running {
			return errors.Join(errInvalidWindowsActivation, err)
		}
	}
	// Existing session/SSH/carrier owners remain independent. New native roles
	// and hostd's live heartbeat must describe the committed installation.
	live := config
	live.Binary, live.TokenFile, live.RuntimeStateRoot = layout.Binary, installed.TokenFile, installed.StateRoot
	live.ActiveVersion, live.Source, live.Pending = installed.Source.Version, installed.Source, false
	status, err := newWindowsSCMActivationBackend(live).activeHostdStatus(ctx)
	if err != nil || !validWindowsRuntimeStatus(status, installed.Source.Version) {
		return errors.Join(errInvalidWindowsActivation, err)
	}
	return nil
}

func validateWindowsNativeInstallJournalBinding(config WindowsConfig, j windowsActivationJournal) error {
	if config.Architecture != j.Architecture || config.Source.Platform != "windows" || config.Source.Architecture != j.Architecture {
		return errInvalidWindowsActivation
	}
	expected := j.PreviousBinary
	if config.Source.Version == j.Version {
		expected = j.Runtime
	} else if config.Source.Version != j.PreviousVersion {
		return errInvalidWindowsActivation
	}
	if config.Source.SHA256 != expected.SHA256 || config.Source.Length != expected.Length {
		return errInvalidWindowsActivation
	}
	return nil
}

// InspectWindowsNativeInstallation derives the existing protected installation
// identity without retiring its updater transaction or stopping services.
func InspectWindowsNativeInstallation(ctx context.Context, ownerSID string, offline bool) (*hostinstall.WindowsRollbackIdentity, []byte, error) {
	identity, _, err := prepareWindowsNativeInstall(ctx, ownerSID, false, offline)
	if err != nil {
		return nil, nil, err
	}
	layout, err := service.WindowsUserLayout(ownerSID)
	if err != nil {
		return nil, nil, err
	}
	journal, err := loadWindowsActivationJournal(WindowsConfig{OwnerSID: ownerSID, StateRoot: layout.UpdateStateRoot})
	if errors.Is(err, os.ErrNotExist) {
		return identity, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	data, err := json.Marshal(journal)
	return identity, data, err
}

// RestoreWindowsNativeInstallationJournal restores only a previously captured
// protected journal at the owning instance path, through its normal ACL writer.
func RestoreWindowsNativeInstallationJournal(ctx context.Context, ownerSID string, identity hostinstall.WindowsRollbackIdentity, data []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(data) == 0 {
		return nil
	}
	if len(data) > 128<<10 {
		return errInvalidWindowsActivation
	}
	var journal windowsActivationJournal
	if err := json.Unmarshal(data, &journal); err != nil {
		return errInvalidWindowsActivation
	}
	layout, err := service.WindowsUserLayout(ownerSID)
	if err != nil {
		return err
	}
	config := WindowsConfig{OwnerSID: ownerSID, StateRoot: layout.UpdateStateRoot}
	if !validWindowsActivationJournal(journal) || !validWindowsActivationPaths(config, journal) || (!nativeWindowsJournalRetirable(journal) && !windowsNativeInstallMaySupersede(journal)) {
		return errInvalidWindowsActivation
	}
	installed, err := hostinstall.LoadWindowsRuntimeConfigForInstance(layout.Instance)
	if err != nil {
		return err
	}
	if windowsNativeInstallMaySupersede(journal) {
		if identity.Signed || identity.Source != installed.Source {
			return errInvalidWindowsActivation
		}
		if err := validateWindowsNativeInstallPreviousSource(installed.Source, journal); err != nil {
			return err
		}
	} else {
		source, signed, err := nativeWindowsRollbackSource(journal)
		source.AutomaticUpdates = identity.Source.AutomaticUpdates
		if err != nil || source != identity.Source || signed != identity.Signed {
			return errInvalidWindowsActivation
		}
	}
	if err := identity.Source.Verify(layout.Binary); err != nil {
		return err
	}
	return (&windowsSCMActivationBackend{config: config}).WriteJournal(journal)
}

func PrepareOfflineWindowsNativeInstall(ctx context.Context, ownerSID string) (*hostinstall.WindowsRollbackIdentity, func() error, error) {
	return prepareWindowsNativeInstall(ctx, ownerSID, true, true)
}
func authorizeWindowsNativeRecovery(ctx context.Context, source workerupdate.TUFSource, identity installsource.Source, offline bool) error {
	if offline {
		return source.AuthorizeOfflineRecovery(ctx, identity.Version, "windows", identity.Architecture)
	}
	return source.AuthorizeRecovery(ctx, identity.Version, "windows", identity.Architecture)
}

// CheckOfflineWindowsNativeInstall admits only a stable completed transaction
// or an untouched staged download. Resuming an interrupted signed activator has
// a separate live-policy contract; an offline supplied operation cannot fetch
// policy or silently bypass that recovery.
func CheckOfflineWindowsNativeInstall(ctx context.Context, config WindowsConfig) error {
	return recoverWindowsNativeInstall(ctx, windowsNativeInstallRecoveryOps{
		offline: true,
		load: func() (windowsActivationJournal, error) {
			path := windowsActivationJournalPath(config.StateRoot)
			if _, err := os.Lstat(path); err != nil {
				return windowsActivationJournal{}, err
			}
			if err := secureWindowsFileShape(path); err != nil {
				return windowsActivationJournal{}, err
			}
			if !windowsMachineFileSecurityMatches(path, "D:P(A;;FA;;;SY)(A;;FA;;;BA)") {
				return windowsActivationJournal{}, errInvalidWindowsActivation
			}
			return loadWindowsActivationJournal(config)
		},
		owner: func(j windowsActivationJournal) (bool, bool, error) {
			return windowsNativeInstallActivatorState(config, j)
		},
	})
}
