package workerupdate

import (
	"context"
	"errors"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/updateflow"
)

// PreparedActivation separates signed artifact approval from native installation.
// Source identifies the extracted executable; Signed and Candidate retain the
// unchanged signed artifact identity, including package and policy metadata.
type PreparedActivation struct {
	Signed     Release
	Candidate  PreparedCandidate
	Executable string
	Source     installsource.Source
	Force      bool
	// Stage runs after verified binary promotion, before workload admission pauses.
	Stage func(context.Context) error
	// Prepare fences feature-worker mutations after hostd has fenced and
	// drained its own admitted operations, before native services stop.
	Prepare func(context.Context) error
	// Validate runs after native readiness and before Source is committed. Its
	// failure must restore the native owner's previous installation.
	Validate func(context.Context) error
}

func (m *Manager) applyPrepared(ctx context.Context, journal updateflow.Journal, signed, payload Release, candidate PreparedCandidate) (Result, error) {
	result := Result{Version: m.active.Version}
	previousExecutable := m.config.RecoveryExecutable
	if previousExecutable == "" {
		previousExecutable = m.config.Binary
	}
	if err := m.verifyRecovery(ctx, m.active, previousExecutable); err != nil {
		return result, err
	}
	source := m.candidateSource(payload)
	activation := PreparedActivation{Signed: signed, Candidate: candidate, Executable: m.config.BinaryStaged, Source: source, Force: m.config.ForceActivation}
	prepared := false
	validationStarted := false
	activation.Prepare = func(prepareCtx context.Context) error {
		if m.config.Gate == nil {
			return ErrInvalidConfig
		}
		prepared = true
		status, err := m.config.Hostd.Active(prepareCtx)
		if err != nil {
			return err
		}
		request := m.gateRequest(journal, signed, status)
		// Probe the existing route before replacement and bind the immutable
		// signed policy. The new runtime is observed separately by Validate.
		if err := m.gate(prepareCtx, releaseDuration(signed.CanaryTimeout, m.config.CanaryTimeout), func(ctx context.Context) error {
			return m.config.Gate.Candidate(ctx, request)
		}); err != nil {
			return err
		}
		return m.gate(prepareCtx, releaseDuration(signed.DrainTimeout, m.config.DrainTimeout), func(ctx context.Context) error {
			return m.config.Gate.Drain(ctx, request)
		})
	}
	activation.Validate = func(validateCtx context.Context) error {
		validationStarted = true
		status, err := m.config.Hostd.Active(validateCtx)
		if err != nil {
			return err
		}
		if !m.validActiveRuntime(status, signed) {
			return ErrInvalidRelease
		}
		observation := journal
		observation.WorkerID, observation.WorkerEpoch = status.WorkerID, status.Epoch
		window := releaseDuration(signed.StabilityWindow, m.config.MonitorWindow)
		interval := releaseDuration(signed.StabilityInterval, m.config.HealthInterval)
		observation.HealthDeadline = m.now().Add(window)
		watchCtx, cancel := context.WithTimeout(validateCtx, window+stabilityCompletionMargin(interval))
		defer cancel()
		healthDone := make(chan error, 1)
		go func() {
			err := m.monitorNative(watchCtx, observation, signed)
			if err != nil {
				cancel()
			}
			healthDone <- err
		}()
		var gateErr error
		if m.config.Gate != nil {
			request := m.gateRequest(journal, signed, status)
			request.Window, request.Interval = window, interval
			gateErr = m.config.Gate.Active(watchCtx, request)
		}
		if gateErr != nil {
			gateErr = errors.Join(ErrActivationGate, gateErr)
			cancel()
		}
		healthErr := <-healthDone
		if err := errors.Join(gateErr, healthErr); err != nil {
			return err
		}
		return nil
	}
	committed, err := m.config.ApplyPrepared(ctx, activation)
	if err != nil {
		// The native journal owns recovery. Keep the exact prepared approval so
		// the protected helper can retry without a second binary rollback owner.
		return m.finishRestored(ctx, journal, signed, committed, prepared, validationStarted, err)
	}
	return m.finishCommitted(ctx, journal, signed, payload, committed)
}

// finishRestored ends an attempt whose native owner restored the previous installation: hostd
// releases the candidate's gate and the journal goes idle with the failure, so no helper start
// retries it. Any other committed source leaves the journal for native recovery.
func (m *Manager) finishRestored(ctx context.Context, journal updateflow.Journal, signed Release, committed installsource.Source, prepared, validationStarted bool, cause error) (Result, error) {
	result := Result{Version: m.active.Version}
	knownRestored := committed.Validate() == nil && committed.Version == m.active.Version && committed.SHA256 == m.active.SHA256 && committed.Length == m.active.Length && committed.Platform == m.active.Platform && committed.Architecture == m.active.Architecture
	if !knownRestored {
		return result, cause
	}
	if verifyErr := committed.Verify(m.config.Binary); verifyErr != nil {
		return result, errors.Join(cause, verifyErr, ErrBlocked)
	}
	if prepared && m.config.Gate != nil {
		recoveryCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), releaseDuration(signed.RollbackTimeout, m.config.RollbackTimeout))
		defer cancel()
		status, statusErr := m.config.Hostd.Active(recoveryCtx)
		if statusErr != nil || !m.validActiveRuntime(status, m.active) {
			return result, errors.Join(cause, statusErr, ErrBlocked)
		}
		request := GateRequest{TransactionID: journal.TransactionID, Previous: signed, Candidate: m.active, Worker: status}
		if rollbackErr := m.config.Gate.Rollback(recoveryCtx, request); rollbackErr != nil {
			return result, errors.Join(cause, rollbackErr, ErrBlocked)
		}
	}
	failure, quarantine := updateflow.FailureDrain, ""
	if validationStarted {
		failure, quarantine = updateflow.FailureHealth, signed.Version
	}
	if idleErr := m.write(m.idleJournal(journal, failure, quarantine)); idleErr != nil {
		return result, errors.Join(cause, idleErr, ErrBlocked)
	}
	return result, cause
}

// candidateSource is the official install identity the native owner commits for payload.
func (m *Manager) candidateSource(payload Release) installsource.Source {
	source := installsource.Source{Version: payload.Version, Platform: payload.Platform, Architecture: payload.Architecture, SHA256: payload.SHA256, Length: payload.Length, Distribution: installsource.Official}
	if m.active.LocalSource != nil {
		source.AutomaticUpdates = m.active.LocalSource.AutomaticUpdates
	}
	return source
}

// finishCommitted completes everything that follows the native commit: the hostd ledger commit,
// manual refresh and the idle journal. It is idempotent, so a helper that restarts after an
// interrupted finish runs it again instead of needing the staged payload, which the commit consumed.
func (m *Manager) finishCommitted(ctx context.Context, journal updateflow.Journal, signed, payload Release, committed installsource.Source) (Result, error) {
	result := Result{Version: m.active.Version}
	source := m.candidateSource(payload)
	if committed.Version != source.Version || committed.Platform != source.Platform || committed.Architecture != source.Architecture || committed.SHA256 != source.SHA256 || committed.Length != source.Length || committed.Distribution != installsource.Official {
		return result, ErrInvalidRelease
	}
	active, err := InstalledBaseline(committed, m.config.Binary)
	if err != nil {
		return result, err
	}
	m.setActive(active)
	if m.config.Gate != nil {
		commitCtx, finish := context.WithTimeout(ctx, 30*time.Second)
		status, statusErr := m.config.Hostd.Active(commitCtx)
		var commitErr error
		if statusErr == nil {
			commitErr = m.config.Gate.Commit(commitCtx, GateRequest{TransactionID: journal.TransactionID, Previous: payload, Candidate: signed, Worker: status})
		}
		finish()
		if err := errors.Join(statusErr, commitErr); err != nil {
			return Result{Version: committed.Version}, m.failCommit(journal, signed, errors.Join(ErrActivationGate, err))
		}
	}
	if m.config.FinalizePrepared != nil {
		if err := m.config.FinalizePrepared(ctx, committed); err != nil {
			return Result{Version: committed.Version}, m.failCommit(journal, signed, err)
		}
	}
	if err := m.write(m.newJournal()); err != nil {
		return Result{Version: committed.Version}, err
	}
	if err := m.removeStaged(); err != nil {
		return Result{Version: committed.Version}, err
	}
	m.record(ctx, EventCommitted, journal, signed, "")
	return Result{Version: committed.Version, Updated: true}, nil
}

// maxCommitAttempts bounds the retries of a failing finish after the native commit.
const maxCommitAttempts = 10

func commitRetryDelay(attempts uint32) time.Duration {
	return min(5*time.Minute, 5*time.Second<<attempts)
}

// failCommit records a failed finish after the native commit. The candidate is installed, so the
// journal keeps the exact approval (the only retry) and schedules the next attempt with backoff;
// after maxCommitAttempts the attempt ends: idle on the installed version, quarantined, with
// FailureCommit so status and doctor say the machine needs attention. It returns cause (joined with
// any journal write failure), never a persisted error text.
func (m *Manager) failCommit(journal updateflow.Journal, signed Release, cause error) error {
	journal.NativeCommitted = true
	journal.CommitAttempts++
	var next updateflow.Journal
	if journal.CommitAttempts >= maxCommitAttempts {
		next = m.idleJournal(journal, updateflow.FailureCommit, signed.Version)
	} else {
		journal.NextCommitRetry, journal.StageUpdatedAt = m.now().Add(commitRetryDelay(journal.CommitAttempts)), m.now()
		next = journal
	}
	if err := m.write(next); err != nil {
		return errors.Join(cause, err)
	}
	return cause
}

// EndRevokedCommittedCandidate ends an approved attempt whose candidate the native owner already installed and
// committed (committed is the installed source) but whose finish current signed policy refuses: it never installs
// a revoked version in either direction, and retrying the same refusal would block every later update. The journal
// goes idle on the installed version, the candidate quarantined, with release_revoked, so status and doctor say
// it needs attention and a newer authorized release supersedes it through the normal download and approval. It
// does nothing, and reports false, unless the journal is that approved candidate and committed is exactly it.
func (m *Manager) EndRevokedCommittedCandidate(ctx context.Context, committed installsource.Source) (bool, error) {
	if ctx == nil {
		return false, ErrInvalidConfig
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	journal, err := updateflow.Load(m.config.StatePath)
	if err != nil {
		return false, err
	}
	if journal.Stage != updateflow.StageAwaitingApproval || journal.ApprovedCandidateID == "" || !m.candidateInstalled(journal) ||
		committed.Version != journal.CandidateVersion || committed.SHA256 != journal.CandidateDigest || committed.Length != journal.CandidateLength || committed.Verify(m.config.Binary) != nil {
		return false, nil
	}
	active, err := InstalledBaseline(committed, m.config.Binary)
	if err != nil {
		return false, err
	}
	m.setActive(active)
	return true, m.write(m.idleJournal(journal, updateflow.FailureRevoked, journal.CandidateVersion))
}

// EndRecoveredCandidate ends an approved attempt whose interrupted native replacement was undone: restored is the
// previous installation that stands again, which the candidate never replaced. The staged payload goes, and the
// journal goes idle on the restored version so no helper start repeats the attempt and a newer release is
// downloaded and approved as an ordinary update. It does nothing, and reports false, unless the journal is that
// approved candidate and restored is exactly the active release.
func (m *Manager) EndRecoveredCandidate(ctx context.Context, restored installsource.Source) (bool, error) {
	if ctx == nil {
		return false, ErrInvalidConfig
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	journal, err := updateflow.Load(m.config.StatePath)
	if err != nil {
		return false, err
	}
	if journal.Stage != updateflow.StageAwaitingApproval || journal.ApprovedCandidateID == "" || m.candidateInstalled(journal) ||
		restored.Version != m.active.Version || restored.SHA256 != m.active.SHA256 || restored.Length != m.active.Length || restored.Verify(m.config.Binary) != nil {
		return false, nil
	}
	if err = m.removeStaged(); err != nil {
		return false, err
	}
	return true, m.write(m.idleJournal(journal, updateflow.FailureCandidate, ""))
}

// MarkActiveRefused records release_revoked on an idle journal whose active release signed policy refuses
// (refused says so), the terminal state status, doctor and the console show as recovery_required: the machine
// runs a version recovery had to put back although it is revoked or below the raised floor, and the update is the
// way forward. The candidate of the ended attempt, if any, stays quarantined. It reports whether it wrote.
func (m *Manager) MarkActiveRefused(ctx context.Context, refused func(Release) bool) (bool, error) {
	if ctx == nil || refused == nil {
		return false, ErrInvalidConfig
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	journal, err := updateflow.Load(m.config.StatePath)
	if err != nil {
		return false, err
	}
	if journal.Stage != updateflow.StageIdle || journal.LastFailure == updateflow.FailureRevoked || !refused(m.active) {
		return false, nil
	}
	return true, m.write(m.idleJournal(journal, updateflow.FailureRevoked, journal.CandidateVersion))
}

// EndStuckAttempt ends an attempt whose activation helper failed for as long as its retry budget allows, so no
// helper start repeats it and uninstall and later updates are no longer held by its handoff. The journal goes
// idle with the candidate quarantined: as rollback_failed (recovery_required in status, doctor and the console)
// when native replacement may have begun (fault), else as an ordinary failed candidate. A journal that already ended
// with a failure, or whose candidate is committed (its finish has its own retry cap), is left alone. It reports
// whether it wrote.
func (m *Manager) EndStuckAttempt(ctx context.Context, fault bool) (bool, error) {
	if ctx == nil {
		return false, ErrInvalidConfig
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	journal, err := updateflow.Load(m.config.StatePath)
	if err != nil {
		return false, err
	}
	if journal.NativeCommitted || journal.Stage == updateflow.StageIdle && (journal.LastFailure != updateflow.FailureNone || !fault) {
		return false, nil
	}
	if err = m.removeStaged(); err != nil {
		return false, err
	}
	failure := updateflow.FailureCandidate
	if fault {
		failure = updateflow.FailureRollback
	}
	return true, m.write(m.idleJournal(journal, failure, journal.CandidateVersion))
}
