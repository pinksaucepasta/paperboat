package updated

import (
	"context"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/workerupdate"
)

// executeWindowsPreparedActivation cannot change services or files: that
// authority belongs to the supplied native lifecycle callback. Its receipt is
// committed only after native health/source commit and the signed gate commit.
func executeWindowsPreparedActivation(ctx context.Context, j windowsActivationJournal, activate func(context.Context, *windowsActivationJournal) error, write func(windowsActivationJournal) error) (windowsActivationJournal, error) {
	if ctx == nil || activate == nil || write == nil || !validWindowsActivationJournal(j) {
		return j, errInvalidWindowsActivation
	}
	if j.Stage == windowsActivationAwaitingApproval || j.ApprovedCandidateID != j.Candidate.ID {
		return j, workerupdate.ErrApprovalRequired
	}
	if j.Stage == windowsActivationRolledBack {
		return j, nil
	}
	if err := activate(ctx, &j); err != nil {
		return j, err
	}
	j.Stage = windowsActivationCommitted
	j.Failure = ""
	return j, write(j)
}

// Runtime configuration may describe candidate code before native health. Only
// its explicit commit, or its authenticated predecessor, is a status authority.
func windowsCommittedSourceProjection(runtime installsource.Source, committed bool, baseline *installsource.Source) installsource.Source {
	if committed {
		return runtime
	}
	if baseline != nil {
		return *baseline
	}
	return installsource.Source{}
}
