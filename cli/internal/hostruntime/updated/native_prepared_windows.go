//go:build windows

package updated

import (
	"context"
	"errors"
	"os"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostdproto"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostinstall"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/workerupdate"
)

func windowsPreparedHelper(config WindowsConfig, j windowsActivationJournal) (string, error) {
	if j.PreviousSource == nil || !validWindowsLocalPrevious(j) {
		return "", errInvalidWindowsActivation
	}
	layout, err := service.WindowsUserLayout(config.OwnerSID)
	if err != nil {
		return "", err
	}
	return hostinstall.WindowsPinnedSourcePath(layout, *j.PreviousSource)
}

func verifyWindowsPreparedRelease(ctx context.Context, config WindowsConfig, j windowsActivationJournal, retainedNativeProof bool) error {
	if !validWindowsActivationJournal(j) || j.PreviousSource == nil {
		return errInvalidWindowsActivation
	}
	source, err := newWindowsTUFSource(config)
	if err != nil {
		return err
	}
	instance, err := service.WindowsUserInstance(config.OwnerSID)
	if err != nil {
		return err
	}
	installed, err := hostinstall.LoadWindowsRuntimeConfigForInstance(instance)
	if err != nil {
		return err
	}
	committedCandidate := installed.Committed && installed.Source.Version == j.Version && installed.Source.SHA256 == j.Runtime.SHA256 && installed.Source.Length == j.Runtime.Length && installed.Source.Distribution == installsource.Official
	nativeBegan := false
	baseline, baselineErr := hostinstall.CapturedWindowsPreparedSource(ctx, config.OwnerSID, j.Candidate.ID)
	if baselineErr == nil {
		if baseline != *j.PreviousSource {
			return errInvalidWindowsActivation
		}
		nativeBegan = true
	} else if !errors.Is(baselineErr, os.ErrNotExist) {
		return baselineErr
	}
	// A queued approval must still describe current signed metadata. Once the
	// protected native receipt exists, recovery retains its original proof; an
	// unrelated latest release never regenerates that approved candidate.
	if !retainedNativeProof && !nativeBegan && !committedCandidate {
		current, eligible, err := source.ResolveManual(ctx)
		if err != nil {
			return err
		}
		if !eligible {
			return workerupdate.ErrPreparedCandidate
		}
		candidate, err := workerupdate.PreparedCandidateForRelease(current)
		if err != nil {
			return err
		}
		if candidate != j.Candidate {
			return workerupdate.ErrPreparedCandidate
		}
	}
	if err := source.AuthorizeRecovery(ctx, j.Version, "windows", j.Architecture); err != nil {
		return err
	}
	return verifyWindowsPreparedCandidate(ctx, j)
}

// The signed receipt owns approval, original policy and handoff only. The native
// lifecycle owns every service/file change and its protected rollback snapshot.
func activateWindowsPreparedNative(ctx context.Context, b *windowsSCMActivationBackend, j windowsActivationJournal) (windowsActivationJournal, error) {
	return executeWindowsPreparedActivation(ctx, j, func(ctx context.Context, receipt *windowsActivationJournal) error {
		// Preserve evidence across Recover removing its completed native journal.
		// Receipt Stage alone never grants this historical-proof exception.
		retainedNativeProof := false
		baseline, baselineErr := hostinstall.CapturedWindowsPreparedSource(ctx, b.config.OwnerSID, receipt.Candidate.ID)
		if baselineErr == nil {
			if receipt.PreviousSource == nil || baseline != *receipt.PreviousSource {
				return errInvalidWindowsActivation
			}
			retainedNativeProof = true
		} else if !errors.Is(baselineErr, os.ErrNotExist) {
			return baselineErr
		}
		if err := verifyWindowsPreparedRelease(ctx, b.config, *receipt, retainedNativeProof); err != nil {
			return err
		}
		policy, err := newWindowsTUFSource(b.config)
		if err != nil {
			return err
		}
		authorize := func(ctx context.Context, identity installsource.Source, pin string) error {
			if err := identity.Verify(pin); err != nil {
				return err
			}
			if identity.Distribution == installsource.Official {
				return policy.AuthorizeRecovery(ctx, identity.Version, "windows", identity.Architecture)
			}
			return nil
		}
		instance, err := service.WindowsUserInstance(b.config.OwnerSID)
		if err != nil {
			return err
		}
		state, err := hostinstall.WindowsPreparedInstallationState(b.config.OwnerSID, receipt.PreviousSource, receipt.Candidate.ID)
		if err != nil {
			return err
		}
		selected := installsource.Source{Version: receipt.Version, Platform: "windows", Architecture: receipt.Architecture, SHA256: receipt.Runtime.SHA256, Length: receipt.Runtime.Length, Distribution: installsource.Official, AutomaticUpdates: receipt.PreviousSource.AutomaticUpdates}
		hooks := hostinstall.PreparedUpdateHooks{DrainTimeout: receipt.Release.DrainTimeout, AuthorizeRestore: authorize}
		hooks.Prepare = func(ctx context.Context) error {
			if err := verifyWindowsPreparedRelease(ctx, b.config, *receipt, retainedNativeProof); err != nil {
				return err
			}
			helper, err := windowsPreparedHelper(b.config, *receipt)
			if err != nil {
				return err
			}
			if err := authorize(ctx, *receipt.PreviousSource, helper); err != nil {
				return err
			}
			target, err := b.featureGate(ctx, *receipt, hostdproto.UpdateGateTarget)
			if err != nil {
				return err
			}
			receipt.GateTarget = &target.Target
			receipt.Stage = windowsActivationSwitching
			if err := b.WriteJournal(*receipt); err != nil {
				return err
			}
			if _, err := b.featureGate(ctx, *receipt, hostdproto.UpdateGateCandidate); err != nil {
				return err
			}
			drained, err := b.featureGate(ctx, *receipt, hostdproto.UpdateGateDrain)
			if err != nil {
				return err
			}
			if drained.BlockedReason != "" {
				return workerupdate.ErrActivationGate
			}
			return nil
		}
		hooks.Validate = func(ctx context.Context) error {
			if err := verifyWindowsPreparedRelease(ctx, b.config, *receipt, retainedNativeProof); err != nil {
				return err
			}
			if receipt.GateTarget == nil {
				return workerupdate.ErrActivationGate
			}
			return b.VerifyFeature(ctx, *receipt)
		}
		// Native recovery/startup must be able to reacquire the updater pipe. The
		// helper remains alive on its immutable previous-source pin throughout.
		if b.closeControl != nil {
			b.controlCloseErr = b.closeControl()
		}
		identity := hostinstall.WindowsRollbackIdentity{Source: *receipt.PreviousSource, Signed: receipt.PreviousSource.Distribution == installsource.Official}
		err = hostinstall.ActivatePreparedWindowsUpdate(ctx, b.config.OwnerSID, receipt.Runtime.Path, selected, receipt.Force, hooks, state, identity)
		if err != nil {
			recoverCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), receipt.Release.RollbackTimeout)
			defer cancel()
			if receipt.GateTarget != nil {
				installed, loadErr := hostinstall.LoadWindowsRuntimeConfigForInstance(instance)
				if loadErr == nil && installed.Committed && installed.Source == *receipt.PreviousSource {
					if _, pendingErr := hostinstall.CapturedWindowsPreparedSource(recoverCtx, b.config.OwnerSID, receipt.Candidate.ID); !errors.Is(pendingErr, os.ErrNotExist) {
						return errors.Join(err, hostinstall.ErrSignedUpdatePending, pendingErr)
					}
					_, gateErr := b.featureGate(recoverCtx, *receipt, hostdproto.UpdateGateRollback)
					if gateErr == nil {
						receipt.Stage = windowsActivationRolledBack
						receipt.Failure = boundedWindowsActivationFailure(err)
					}
					err = errors.Join(err, gateErr, b.WriteJournal(*receipt))
					if gateErr == nil {
						err = errors.Join(err, b.Quarantine(recoverCtx, *receipt))
					}
				} else {
					err = errors.Join(err, loadErr)
				}
			}
			return err
		}
		receipt.Stage = windowsActivationCommitReady
		if err := b.WriteJournal(*receipt); err != nil {
			return err
		}
		if _, err := b.featureGate(ctx, *receipt, hostdproto.UpdateGateCommit); err != nil {
			return err
		}
		return b.clearCommittedOwnerMaintenance(*receipt)
	}, b.WriteJournal)
}

func clearWindowsPreparedReceipt(ctx context.Context, config WindowsConfig, j windowsActivationJournal) error {
	if j.Stage != windowsActivationCommitted && j.Stage != windowsActivationRolledBack {
		return errInvalidWindowsActivation
	}
	if err := verifyWindowsTerminalNativeState(ctx, config, j); err != nil {
		return err
	}
	current, err := loadWindowsActivationJournal(config)
	if err != nil {
		return err
	}
	if current.TransactionID != j.TransactionID || current.Candidate != j.Candidate || current.Stage != j.Stage {
		return errInvalidWindowsActivation
	}
	err = os.Remove(windowsActivationJournalPath(config.StateRoot))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	// The native installation owns active Source; no version pointer is written.
	return nil
}
