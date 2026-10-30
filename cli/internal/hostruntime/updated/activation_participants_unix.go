//go:build darwin || linux

package updated

import (
	"context"
	"errors"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/workerupdate"
)

// applyPrepared lets the native transaction validate the per-user routing
// daemon as well as hostd/updater. Terminal owners retain independent lifetimes.
func (s *Service) applyPrepared(ctx context.Context, candidate workerupdate.PreparedActivation) (installsource.Source, error) {
	handoff, err := readUnixHandoff(s.config.StateRoot)
	if err != nil {
		return installsource.Source{}, err
	}
	if handoff == nil || candidate.Validate == nil || s.config.LoadInstalledSource == nil {
		return installsource.Source{}, ErrInvalidConfig
	}
	prepare := candidate.Prepare
	if prepare == nil {
		return installsource.Source{}, ErrInvalidConfig
	}
	routingTouched := false
	candidate.Stage = func(prepareCtx context.Context) error {
		probe, err := s.config.Participants.Probe(prepareCtx)
		if err != nil {
			return participantFailure("probe", errors.Join(ErrParticipantReadiness, err))
		}
		if probe.Running {
			handoff.Baseline.Running = true
		}
		if probe.State == "ready" || probe.Machines > 0 {
			handoff.RequireInventory = true
		}
		if err := unixParticipantReady(probe, handoff.Previous.Version, handoff.Baseline, handoff.RequireInventory); err != nil {
			return err
		}
		if err := writeUnixHandoff(s.config.StateRoot, handoff); err != nil {
			return err
		}
		if handoff.Baseline.Running {
			routingTouched = true
			if err := s.config.Participants.Restart(prepareCtx); err != nil {
				return participantFailure("restart_daemon", errors.Join(ErrParticipantReadiness, err))
			}
		}
		return nil
	}
	candidate.Prepare = prepare
	validate := candidate.Validate
	readiness := &unixParticipantReadiness{participants: s.config.Participants, handoff: handoff}
	timeout := handoff.RecoveryTimeout
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	candidate.Validate = func(validateCtx context.Context) error {
		if handoff.Baseline.Running {
			routingTouched = true
			// A retry after Source commit can already have the exact routing daemon.
			probe, probeErr := s.config.Participants.Probe(validateCtx)
			if probeErr != nil || !probe.Running || probe.Version != candidate.Source.Version {
				if err := s.config.Participants.Restart(validateCtx); err != nil {
					return participantFailure("restart_daemon", errors.Join(ErrParticipantReadiness, err))
				}
			}
		}
		readyCtx, cancel := context.WithTimeout(validateCtx, timeout)
		readyErr := readiness.waitReady(readyCtx, candidate.Source.Version, true)
		cancel()
		if readyErr != nil {
			return readyErr
		}
		if err := validate(validateCtx); err != nil {
			return err
		}
		probe, err := s.config.Participants.Probe(validateCtx)
		if err != nil {
			return participantFailure("probe", errors.Join(ErrParticipantReadiness, err))
		}
		return unixParticipantReady(probe, candidate.Source.Version, handoff.Baseline, handoff.RequireInventory)
	}
	committed, err := s.config.ApplyPrepared(ctx, candidate)
	if err == nil {
		return committed, nil
	}
	if errors.Is(err, service.ErrLifecycleUncertain) {
		return installsource.Source{}, errors.Join(err, workerupdate.ErrBlocked)
	}
	// The native owner must have restored and re-authorized the exact baseline
	// before a user daemon can restart against it. Pending recovery fails closed.
	source, sourceErr := s.config.LoadInstalledSource()
	if sourceErr != nil || source.Version != handoff.Previous.Version || source.SHA256 != handoff.Previous.SHA256 || source.Length != handoff.Previous.Length {
		return installsource.Source{}, errors.Join(err, sourceErr, workerupdate.ErrBlocked)
	}
	recoveryCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	defer cancel()
	if verifyErr := source.Verify(s.config.Binary); verifyErr != nil {
		return installsource.Source{}, errors.Join(err, verifyErr, workerupdate.ErrBlocked)
	}
	if !routingTouched {
		return source, err
	}
	restartErr := s.config.Participants.Restart(recoveryCtx)
	if restartErr != nil {
		return installsource.Source{}, errors.Join(err, participantFailure("restart_daemon", errors.Join(ErrParticipantReadiness, restartErr)))
	}
	if readinessErr := readiness.waitReady(recoveryCtx, source.Version, false); readinessErr != nil {
		return installsource.Source{}, errors.Join(err, readinessErr)
	}
	return source, err
}

// recoverPrepared runs the native recovery of an installation interrupted before its commit. When
// that restored the previous version, the routing daemon restarts on it, as after a rolled-back
// attempt, before the attempt ends.
func (s *Service) recoverPrepared(ctx context.Context) (installsource.Source, error) {
	handoff, err := readUnixHandoff(s.config.StateRoot)
	if err != nil {
		return installsource.Source{}, err
	}
	if handoff == nil {
		return installsource.Source{}, ErrInvalidConfig
	}
	committed, err := s.config.RecoverPrepared(ctx)
	if err != nil {
		return installsource.Source{}, err
	}
	previous := handoff.Previous
	if committed.Version != previous.Version || committed.SHA256 != previous.SHA256 || committed.Length != previous.Length {
		return committed, nil
	}
	timeout := handoff.RecoveryTimeout
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	recoveryCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	defer cancel()
	if handoff.Baseline.Running {
		if err := s.config.Participants.Restart(recoveryCtx); err != nil {
			return installsource.Source{}, participantFailure("restart_daemon", errors.Join(ErrParticipantReadiness, err))
		}
	}
	readiness := &unixParticipantReadiness{participants: s.config.Participants, handoff: handoff}
	if err := readiness.waitReady(recoveryCtx, committed.Version, false); err != nil {
		return installsource.Source{}, err
	}
	return committed, nil
}

// resolveUnixPreparedRelease keeps ordinary queued approval pinned to the current
// candidate; only protected native progress permits resuming its original bytes.
