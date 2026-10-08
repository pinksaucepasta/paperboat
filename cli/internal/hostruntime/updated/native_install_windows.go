//go:build windows

package updated

import (
	"context"
	"errors"
	"fmt"
	"github.com/pinksaucepasta/paperboat/internal/windows/elevation"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostinstall"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/workerupdate"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

// PrepareWindowsNativeInstall is called with the instance installation mutex
// held, after synchronously stopping the updater. It retires only completed
// transactions; the returned callback restores their policy state on failure,
// before the prior services restart. Signed metadata and release slots remain.
func PrepareWindowsNativeInstall(ctx context.Context, ownerSID string) (*hostinstall.WindowsRollbackIdentity, func() error, error) {
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
	defer manager.Disconnect()
	updater, err := manager.OpenService(windowsUpdaterService + "-" + layout.Instance)
	if err == nil {
		status, queryErr := updater.Query()
		updater.Close()
		if queryErr != nil {
			return nil, nil, queryErr
		}
		if status.State != svc.Stopped {
			return nil, nil, fmt.Errorf("stop the updater before reinstalling: %w", ErrWindowsActivationUnavailable)
		}
	} else if !errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		return nil, nil, err
	}
	activator, err := manager.OpenService(windowsActivatorService + "-" + layout.Instance)
	if err == nil {
		activator.Close()
		return nil, nil, fmt.Errorf("finish or recover the pending update before reinstalling: %w", ErrWindowsActivationUnavailable)
	}
	if !errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		return nil, nil, err
	}
	updateConfig := WindowsConfig{OwnerSID: ownerSID, StateRoot: layout.UpdateStateRoot}
	journal, err := loadWindowsActivationJournal(updateConfig)
	if errors.Is(err, os.ErrNotExist) {
		if err := config.Source.Verify(layout.Binary); err == nil {
			return &hostinstall.WindowsRollbackIdentity{Source: config.Source}, noop, nil
		}
		// Native repair can select the previously authenticated signed slot.
		// Its durable rollback identity remains the proof after journal retirement.
		if !config.RollbackSigned || config.RollbackSource == nil {
			return nil, nil, errInvalidWindowsActivation
		}
		identity := *config.RollbackSource
		if err := identity.Verify(layout.Binary); err != nil {
			return nil, nil, err
		}
		source := workerupdate.TUFSource{RepositoryURL: config.Artifact.RepositoryURL, StateRoot: filepath.Join(layout.UpdateStateRoot, "tuf")}
		if err := source.AuthorizeRecovery(ctx, identity.Version, "windows", identity.Architecture); err != nil {
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
	if !nativeWindowsJournalRetirable(journal) {
		return nil, nil, fmt.Errorf("finish or recover the pending update before reinstalling: %w", ErrWindowsActivationUnavailable)
	}
	sourceIdentity, signed, err := nativeWindowsRollbackSource(journal)
	if err != nil {
		return nil, nil, err
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
		if err := source.AuthorizeRecovery(ctx, sourceIdentity.Version, "windows", sourceIdentity.Architecture); err != nil {
			return nil, nil, err
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if err := os.Remove(windowsActivationJournalPath(layout.UpdateStateRoot)); err != nil {
		return nil, nil, err
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
			// Retirement retries reuse the verified registered activator. The general
			// resume predicate deliberately excludes terminal journals.
			if nativeWindowsJournalRetirable(j) {
				return startWindowsActivatorService(config.OwnerSID)
			}
			_, err := startVerifiedWindowsActivation(ctx, config, j)
			return err
		},
	}
	return recoverWindowsNativeInstall(ctx, ops)
}

// These operations isolate the native boundary for transaction/recovery tests;
// the public entry above always supplies the protected validators and SCM owner.
type windowsNativeInstallRecoveryOps struct {
	load             func() (windowsActivationJournal, error)
	validate         func(context.Context, windowsActivationJournal) error
	validateMutation func(context.Context, windowsActivationJournal) error
	owner            func(windowsActivationJournal) (registered, running bool, err error)
	stopUpdater      func(context.Context) error
	resume           func(context.Context, windowsActivationJournal) error
}

func recoverWindowsNativeInstall(ctx context.Context, ops windowsNativeInstallRecoveryOps) error {
	bounded, cancel := context.WithTimeout(ctx, elevation.RuntimeInstallRecoveryDuration)
	defer cancel()
	if err := bounded.Err(); err != nil {
		return err
	}
	initial, err := ops.load()
	if errors.Is(err, os.ErrNotExist) {
		registered, _, ownerErr := ops.owner(windowsActivationJournal{})
		if ownerErr != nil {
			return ownerErr
		}
		if registered {
			return errInvalidWindowsActivation
		}
		return nil
	}
	if err != nil {
		return err
	}
	if !validWindowsActivationJournal(initial) {
		return errInvalidWindowsActivation
	}
	if initial.Stage == windowsActivationAwaitingApproval || initial.ApprovedCandidateID != initial.Candidate.ID {
		return fmt.Errorf("approve the pending update before reinstalling: %w", ErrApprovalRequired)
	}
	if err := ops.validate(bounded, initial); err != nil {
		return err
	}
	journal := initial
	started := false
	for {
		if err := bounded.Err(); err != nil {
			return err
		}
		if journal.TransactionID != initial.TransactionID || journal.Candidate.ID != initial.Candidate.ID {
			return errInvalidWindowsActivation
		}
		registered, running, err := ops.owner(journal)
		if err != nil {
			return err
		}
		if nativeWindowsJournalRetirable(journal) && !registered {
			return nil
		}
		if !running && !started {
			if err := ops.validateMutation(bounded, journal); err != nil {
				return err
			}
			// Only Updated is stopped. Hostd, SSH and the local daemon continue to own
			// their workloads until the transaction's signed policy decides otherwise.
			if err := ops.stopUpdater(bounded); err != nil {
				return err
			}
			if err := ops.resume(bounded, journal); err != nil {
				return err
			}
			started = true
		}
		timer := time.NewTimer(250 * time.Millisecond)
		select {
		case <-bounded.Done():
			timer.Stop()
			return bounded.Err()
		case <-timer.C:
		}
		journal, err = ops.load()
		if err != nil {
			return err
		}
	}
}

func windowsNativeInstallActivatorState(config WindowsConfig, j windowsActivationJournal) (bool, bool, error) {
	instance, _, _, _, name, err := windowsInstanceNames(config.OwnerSID)
	if err != nil {
		return false, false, err
	}
	manager, err := mgr.Connect()
	if err != nil {
		return false, false, err
	}
	defer manager.Disconnect()
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
	defer item.Close()
	definition, err := item.Config()
	if err != nil {
		return true, false, err
	}
	arguments, err := windows.DecomposeCommandLine(definition.BinaryPathName)
	expected := []string{j.Updater.Path, "daemon", "__runtime-activate", "--instance", instance}
	if err != nil || len(arguments) != len(expected) || !strings.EqualFold(arguments[0], expected[0]) || !slices.Equal(arguments[1:], expected[1:]) || !validPrivilegedWindowsServiceConfig(definition, mgr.StartAutomatic, mgr.ErrorSevere) {
		return true, false, errInvalidWindowsActivation
	}
	if err := validateWindowsRecovery(item); err != nil {
		return true, false, err
	}
	status, err := item.Query()
	if err != nil {
		return true, false, err
	}
	return true, status.State != svc.Stopped, nil
}

func verifyWindowsNativeInstallRolePins(ctx context.Context, config WindowsConfig, j windowsActivationJournal) error {
	layout, err := service.WindowsUserLayout(config.OwnerSID)
	if err != nil {
		return err
	}
	hostdName, updaterName, err := windowsInstanceServiceNames(config.OwnerSID)
	if err != nil {
		return err
	}
	for _, role := range []struct {
		name, kind, argument string
		old, new             windowsServiceTarget
	}{
		{hostdName, service.HostdKind, "__runtime-hostd", j.OldHostd, j.NewHostd},
		{updaterName, service.UpdaterKind, "__runtime-updated", j.OldUpdater, j.NewUpdater},
	} {
		actual, err := queryWindowsServiceTarget(role.name, role.argument)
		if err != nil {
			return err
		}
		identity, err := service.VerifyOwnedWindowsRoleExecutable(role.kind, layout.Instance, actual.Executable)
		if err != nil {
			return err
		}
		if !windowsOwnedServiceExecutable(layout, actual.Executable) || !windowsNativeInstallPinMatches(role.old, actual, identity) && !windowsNativeInstallPinMatches(role.new, actual, identity) {
			return errInvalidWindowsActivation
		}
		target := workerupdate.ComponentTarget{SHA256: identity.SHA256, Length: identity.Length, Platform: "windows", Architecture: j.Architecture}
		if err := verifyWindowsStableBinary(ctx, actual.Executable, target, config.OwnerSID); err != nil {
			return err
		}
	}
	return nil
}

func windowsNativeInstallPinMatches(expected, actual windowsServiceTarget, identity service.WindowsExecutableIdentity) bool {
	return strings.EqualFold(expected.Executable, actual.Executable) && strings.EqualFold(identity.Executable, actual.Executable) && slices.Equal(expected.Arguments, actual.Arguments) && expected.SHA256 == identity.SHA256 && expected.Length == identity.Length && expected.Length > 0
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
