//go:build darwin || linux

package hostruntimecmd

import (
	"context"
	"errors"
	"runtime"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostinstall"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/updated"
)

var suppliedUpdateReadyTimeout = 60 * time.Second

// suppliedUpdateValidator proves the user's daemon healthy on the new version before a supplied
// installation is committed, as signed updates do. The daemon is restarted on the new executable and
// must report that version (and be ready, unless it was already unhealthy before for a local reason). A
// machine without a running user daemon has nothing to prove. Its baseline is taken now, before anything
// changes.
func suppliedUpdateValidator(ctx context.Context, request hostinstall.Request) (validate, recover func(context.Context) error) {
	layout, err := service.UserLayout(runtime.GOOS, request.UID)
	if err != nil {
		return nil, nil
	}
	participants, err := newUnixUpdateParticipants(layout.Binary, request.UID, request.GID)
	if err != nil {
		return nil, nil
	}
	baseline, baselineErr := participants.Probe(ctx)
	if baselineErr != nil || !baseline.Running {
		return nil, nil
	}
	return daemonValidation(participants, request.Source.Version, baseline)
}

// daemonValidation returns the validation hook and the step that follows a failed install. Validation
// restarts the user's daemon on the new executable; when the install then fails, the previous files are
// restored but that daemon kept running the replaced binary against the restored runtime (lanel-b,
// 2026-10-22: daemon on the new version, runtime on the old, "Local machine access is unavailable").
// recover restarts it once more on the restored executable, and does nothing if validation never
// restarted it.
func daemonValidation(participants updated.UnixParticipants, version string, baseline updated.UnixParticipantProbe) (validate, recover func(context.Context) error) {
	restarted := false
	validate = func(ctx context.Context) error {
		restarted = true
		return restartAndAwaitDaemon(ctx, participants, version, baseline)
	}
	recover = func(ctx context.Context) error {
		if !restarted {
			return nil
		}
		restartCtx, cancel := context.WithTimeout(ctx, suppliedUpdateReadyTimeout)
		defer cancel()
		if err := participants.Restart(restartCtx); err != nil {
			return errors.Join(errors.New("the user's Paperboat daemon could not be restarted on the restored version; run pb doctor"), err)
		}
		return nil
	}
	return validate, recover
}

func restartAndAwaitDaemon(ctx context.Context, participants updated.UnixParticipants, version string, baseline updated.UnixParticipantProbe) error {
	if err := participants.Restart(ctx); err != nil {
		return errors.Join(errors.New("the user's Paperboat daemon could not be restarted on the new version"), err)
	}
	readyCtx, cancel := context.WithTimeout(ctx, suppliedUpdateReadyTimeout)
	defer cancel()
	requireInventory := baseline.Machines > 0 || baseline.State == "ready"
	var last error
	waitingForControlPlane := false
	delay := 250 * time.Millisecond
	for {
		probe, err := participants.Probe(readyCtx)
		if err == nil {
			if last = updated.ParticipantReady(probe, version, baseline, requireInventory); last == nil {
				return nil
			}
			// The new daemon runs but is degraded. Only the control plane not answering yet is worth
			// waiting for (a slow server makes the first refresh fail); an update is never committed on
			// that state, because a version that cannot talk to the server looks the same. Any other
			// degradation fails the validation now.
			waitingForControlPlane = probe.Running && probe.Version == version && probe.State == "degraded" && probe.ControlPlaneUnavailableOnly
			if probe.Running && probe.Version == version && probe.State == "degraded" && !probe.ControlPlaneUnavailableOnly {
				return errors.Join(errors.New("the user's Paperboat daemon came up degraded on the new version"), last)
			}
		} else {
			last = err
		}
		select {
		case <-readyCtx.Done():
			if waitingForControlPlane {
				return errors.Join(errDaemonControlPlaneTimeout, last, readyCtx.Err())
			}
			return errors.Join(errors.New("the user's Paperboat daemon did not become ready on the new version"), last, readyCtx.Err())
		case <-time.After(delay):
		}
		delay = min(2*delay, 2*time.Second)
	}
}

// errDaemonControlPlaneTimeout reports a validation that ended while the new daemon was still waiting
// for the Paperboat server: the previous version is restored and the update can simply be retried.
var errDaemonControlPlaneTimeout = errors.New("the Paperboat server did not answer the new version's daemon before the update's deadline, so the previous version was restored; retry the update when the server is reachable")
