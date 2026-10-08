package updated

import (
	"context"
	"errors"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/workerupdate"
)

// Ordinary feature replacement uses the existing transaction journal while
// keeping every SCM process owner alive. All recovery restores trusted feature
// code and canonical CLI bytes; it never terminates owner processes.
func executeWindowsFeatureActivation(ctx context.Context, b windowsActivationBackend, f windowsFeatureActivationBackend, j windowsActivationJournal) (windowsActivationJournal, error) {
	if j.Stage == windowsActivationAwaitingApproval || j.ApprovedCandidateID != j.Candidate.ID {
		return j, workerupdate.ErrApprovalRequired
	}
	if j.Stage == windowsActivationCommitted || j.Stage == windowsActivationCommitReady {
		if err := b.VerifyCommitted(ctx, j); err != nil {
			return j, err
		}
		j.Stage = windowsActivationCommitted
		if err := b.WriteJournal(j); err != nil {
			return j, err
		}
		return j, f.CompleteFeature(ctx, j)
	}
	if j.Stage == windowsActivationRolledBack {
		return j, nil
	}
	if j.Stage != windowsActivationStaged {
		return rollbackWindowsFeature(ctx, b, f, j, errors.New("interrupted feature replacement recovered"))
	}
	if err := b.AuthorizeRecovery(ctx, j); err != nil {
		return j, err
	}
	target, err := f.PrepareFeature(ctx, j)
	if err != nil {
		if target.Validate() == nil {
			j.GateTarget = &target
		}
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), j.Release.RollbackTimeout)
		defer cancel()
		return j, errors.Join(err, f.AbortFeature(cleanupCtx, j))
	}
	j.GateTarget = &target
	j.Stage = windowsActivationSwitching
	if err = b.WriteJournal(j); err != nil {
		return rollbackWindowsFeature(ctx, b, f, j, err)
	}
	if err = f.ActivateFeature(ctx, j); err != nil {
		return rollbackWindowsFeature(ctx, b, f, j, err)
	}
	j.Stage = windowsActivationServicesLive
	if err = b.WriteJournal(j); err != nil {
		return rollbackWindowsFeature(ctx, b, f, j, err)
	}
	if err = f.VerifyFeature(ctx, j); err != nil {
		return rollbackWindowsFeature(ctx, b, f, j, err)
	}
	if err = b.CommitCLI(ctx, j); err != nil {
		return rollbackWindowsFeature(ctx, b, f, j, err)
	}
	j.Stage = windowsActivationCommitReady
	if err = b.WriteJournal(j); err != nil {
		return j, err
	}
	return executeWindowsFeatureActivation(ctx, b, f, j)
}
func rollbackWindowsFeature(ctx context.Context, b windowsActivationBackend, f windowsFeatureActivationBackend, j windowsActivationJournal, cause error) (windowsActivationJournal, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), j.Release.RollbackTimeout)
	defer cancel()
	if err := b.AuthorizeRecovery(ctx, j); err != nil {
		return j, errors.Join(cause, err)
	}
	j.Stage = windowsActivationRollingBack
	j.Failure = boundedWindowsActivationFailure(cause)
	if err := b.WriteJournal(j); err != nil {
		return j, errors.Join(cause, err)
	}
	if err := f.RestoreFeature(ctx, j); err != nil {
		return j, errors.Join(cause, err)
	}
	if err := b.CommitCLI(ctx, windowsActivationJournal{Version: j.PreviousVersion}); err != nil {
		return j, errors.Join(cause, err)
	}
	if err := b.VerifyRollback(ctx, j); err != nil {
		return j, errors.Join(cause, err)
	}
	j.Stage = windowsActivationRolledBack
	if err := b.WriteJournal(j); err != nil {
		return j, errors.Join(cause, err)
	}
	return j, errors.Join(cause, b.Quarantine(ctx, j))
}
