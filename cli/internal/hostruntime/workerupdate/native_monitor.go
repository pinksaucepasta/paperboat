package workerupdate

import (
	"context"
	"errors"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostdproto"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/updateflow"
)

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
