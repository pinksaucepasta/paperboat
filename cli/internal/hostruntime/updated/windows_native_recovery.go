package updated

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/windows/elevation"
)

// These operations isolate the native boundary for transaction/recovery tests;
// the public entry above always supplies the protected validators and SCM owner.
type windowsNativeInstallRecoveryOps struct {
	offline          bool
	load             func() (windowsActivationJournal, error)
	validate         func(context.Context, windowsActivationJournal) error
	validateMutation func(context.Context, windowsActivationJournal) error
	owner            func(windowsActivationJournal) (registered, running bool, err error)
	stopUpdater      func(context.Context) error
	resume           func(context.Context, windowsActivationJournal) error
	finishTerminal   func(context.Context, windowsActivationJournal) error
	validateTerminal func(context.Context, windowsActivationJournal) error
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
	if ops.offline {
		if !nativeWindowsJournalRetirable(initial) && !windowsNativeInstallMaySupersede(initial) {
			return ErrWindowsActivationUnavailable
		}
		registered, running, err := ops.owner(initial)
		if err != nil {
			return err
		}
		if registered || running {
			return ErrWindowsActivationUnavailable
		}
		return nil
	}
	if windowsNativeInstallMaySupersede(initial) {
		if err := ops.validate(bounded, initial); err != nil {
			return err
		}
		registered, running, err := ops.owner(initial)
		if err != nil {
			return err
		}
		if registered || running {
			return ErrWindowsActivationUnavailable
		}
		// PrepareWindowsNativeInstall snapshots/removes this journal only after
		// the updater has stopped, and restores it if installation fails.
		return nil
	}
	if initial.Stage == windowsActivationAwaitingApproval || initial.ApprovedCandidateID != initial.Candidate.ID {
		return fmt.Errorf("approve the pending update before reinstalling: %w", ErrApprovalRequired)
	}
	if err := ops.validate(bounded, initial); err != nil {
		return err
	}
	journal := initial
	started := false
	terminalFinished := false
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
			if ops.validateTerminal == nil {
				return errInvalidWindowsActivation
			}
			return ops.validateTerminal(bounded, journal)
		}
		if !running && nativeWindowsJournalRetirable(journal) && !terminalFinished {
			if err := ops.validateMutation(bounded, journal); err != nil {
				return err
			}
			// Validation may take time. Never steal a service that acquired the
			// transaction while those checks were running.
			_, running, err = ops.owner(journal)
			if err != nil {
				return err
			}
			if !running {
				if err := ops.finishTerminal(bounded, journal); err != nil {
					return err
				}
				terminalFinished = true
			}
		} else if !running && !started && !nativeWindowsJournalRetirable(journal) {
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

var ErrWindowsActivationUnavailable = errors.New("Windows updater activation is unavailable")

func finishWindowsActivatorResult(ctx context.Context, journal windowsActivationJournal, activationErr error, restoreUpdater func(context.Context, windowsActivationJournal) error, retire func() error) error {
	terminal := journal.Stage == windowsActivationCommitted && activationErr == nil || journal.Stage == windowsActivationRolledBack
	recoveryTimeout := journal.Release.RollbackTimeout
	if recoveryTimeout <= 0 {
		recoveryTimeout = 30 * time.Second
	}
	recoveryCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), recoveryTimeout)
	defer cancel()
	if err := restoreUpdater(recoveryCtx, journal); err != nil {
		return errors.Join(activationErr, err)
	}
	if !terminal {
		return activationErr
	}
	// Rolled-back transactions intentionally retain the original failure in
	// their journal. It is no longer a service failure after usable recovery.
	return retire()
}
