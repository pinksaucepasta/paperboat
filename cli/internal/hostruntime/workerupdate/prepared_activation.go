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
	if err := m.authorizeRecovery(ctx, m.active, previousExecutable); err != nil {
		return result, err
	}
	source := installsource.Source{Version: payload.Version, Platform: payload.Platform, Architecture: payload.Architecture, SHA256: payload.SHA256, Length: payload.Length, Distribution: installsource.Official}
	if m.active.LocalSource != nil {
		source.AutomaticUpdates = m.active.LocalSource.AutomaticUpdates
	}
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
		knownRestored := committed.Validate() == nil && committed.Version == m.active.Version && committed.SHA256 == m.active.SHA256 && committed.Length == m.active.Length && committed.Platform == m.active.Platform && committed.Architecture == m.active.Architecture
		if knownRestored {
			if verifyErr := committed.Verify(m.config.Binary); verifyErr != nil {
				return result, errors.Join(err, verifyErr, ErrBlocked)
			}
		}
		if knownRestored && prepared && m.config.Gate != nil {
			recoveryCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), releaseDuration(signed.RollbackTimeout, m.config.RollbackTimeout))
			defer cancel()
			status, statusErr := m.config.Hostd.Active(recoveryCtx)
			if statusErr != nil || !m.validActiveRuntime(status, m.active) {
				return result, errors.Join(err, statusErr, ErrBlocked)
			}
			request := GateRequest{TransactionID: journal.TransactionID, Previous: signed, Candidate: m.active, Worker: status}
			if rollbackErr := m.config.Gate.Rollback(recoveryCtx, request); rollbackErr != nil {
				return result, errors.Join(err, rollbackErr, ErrBlocked)
			}
		}
		if knownRestored {
			failure, quarantine := updateflow.FailureDrain, ""
			if validationStarted {
				failure, quarantine = updateflow.FailureHealth, signed.Version
			}
			if idleErr := m.write(m.idleJournal(journal, failure, quarantine)); idleErr != nil {
				return result, errors.Join(err, idleErr, ErrBlocked)
			}
		}
		return result, err
	}
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
			return Result{Version: committed.Version}, errors.Join(ErrActivationGate, err)
		}
	}
	if m.config.FinalizePrepared != nil {
		if err := m.config.FinalizePrepared(ctx, committed); err != nil {
			return Result{Version: committed.Version}, err
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
