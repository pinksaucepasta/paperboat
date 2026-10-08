package workerupdate

import (
	"context"
	"errors"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostdproto"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/updateflow"
)

// activatePreparedNative uses the explicitly approved interruption boundary.
// Native service replacement needs no parallel worker, canary route or drain grant.
func (m *Manager) activatePreparedNative(ctx context.Context, j updateflow.Journal, signed, local Release) (result Result, activationErr error) {
	result = Result{Version: m.active.Version}
	if err := m.authorizeRecovery(ctx, m.active, m.config.Binary); err != nil {
		return result, err
	}
	if m.config.AuthorizeOwnerMaintenance != nil {
		if err := m.config.AuthorizeOwnerMaintenance(ctx, signed, m.config.ManualActivation); err != nil {
			return result, err
		}
	}
	ownerStopped := false
	defer func() {
		if !ownerStopped && m.config.AbortOwnerMaintenance != nil {
			cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			activationErr = errors.Join(activationErr, m.config.AbortOwnerMaintenance(cleanupCtx))
		}
	}()
	j.NativeActivation = true
	var err error
	j, err = m.transition(j, updateflow.StageCutover)
	if err != nil {
		return result, err
	}
	if err = m.write(j); err != nil {
		return result, err
	}
	m.record(ctx, EventActivating, j, signed, "")
	if err = m.promoteStorage(); err != nil {
		return result, m.restoreUnstoppedOwner(ctx, j, signed, err)
	}
	j.StagedPath = m.config.Binary
	if err = m.write(j); err != nil {
		return result, m.restoreUnstoppedOwner(ctx, j, signed, err)
	}
	ownerStopped = true
	active, err := m.activateRuntime(ctx, signed)
	if err != nil || !m.validActiveRuntime(active, signed, "", 0) {
		if err == nil {
			err = ErrInvalidRelease
		}
		return result, m.restoreCanonicalRuntime(ctx, j, signed, err)
	}
	j.WorkerID, j.WorkerEpoch = active.WorkerID, active.Epoch
	j, err = m.transition(j, updateflow.StageMonitoring)
	if err != nil {
		return result, m.restoreCanonicalRuntime(ctx, j, signed, err)
	}
	j.HealthDeadline = m.now().Add(releaseDuration(signed.StabilityWindow, m.config.MonitorWindow))
	if err = m.write(j); err != nil {
		return result, m.restoreCanonicalRuntime(ctx, j, signed, err)
	}
	if err = m.monitorNative(ctx, j, signed); err != nil {
		return result, m.rollbackActive(ctx, j, signed, nil, err)
	}
	j, err = m.transition(j, updateflow.StageCommitted)
	if err != nil {
		return result, err
	}
	if err = m.write(j); err != nil {
		return result, err
	}
	if err = m.commitGate(ctx, j, local); err != nil {
		return Result{Version: local.Version}, err
	}
	m.setActive(local)
	if err = m.finishCommitted(j); err != nil {
		return Result{Version: local.Version}, err
	}
	m.record(ctx, EventCommitted, j, signed, "")
	return Result{Version: local.Version, Updated: true}, nil
}

func (m *Manager) restoreUnstoppedOwner(ctx context.Context, j updateflow.Journal, release Release, cause error) error {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), m.config.RollbackTimeout)
	defer cancel()
	if err := m.authorizeStorageRestore(cleanupCtx, m.active); err != nil {
		return errors.Join(cause, err, ErrBlocked)
	}
	if err := m.restoreStorage(); err != nil {
		cause = errors.Join(cause, errStorageRestoreFailed, err)
	}
	return m.restoreAfterDrain(cleanupCtx, j, release, nil, cause)
}

func (m *Manager) monitorNative(ctx context.Context, j updateflow.Journal, release Release) error {
	if j.HealthDeadline.IsZero() {
		return ErrBlocked
	}
	interval := releaseDuration(release.StabilityInterval, m.config.HealthInterval)
	remaining := j.HealthDeadline.Sub(m.now())
	deadline := j.HealthDeadline.Add(stabilityCompletionMargin(interval)).Sub(m.now())
	if deadline <= 0 {
		return errors.Join(ErrActivationGate, context.DeadlineExceeded)
	}
	checkCtx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()
	probe := func() error {
		status, err := m.config.Hostd.Active(checkCtx)
		if err != nil {
			return err
		}
		if !matches(status, hostdproto.StateActive, j.WorkerID, j.WorkerEpoch) {
			return ErrInvalidRelease
		}
		return m.config.Health.Check(checkCtx, status, release)
	}
	if err := probe(); err != nil {
		return err
	}
	timer := time.NewTimer(max(remaining, 0))
	defer timer.Stop()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-checkCtx.Done():
			return checkCtx.Err()
		case <-timer.C:
			return probe()
		case <-ticker.C:
			if err := probe(); err != nil {
				return err
			}
		}
	}
}
