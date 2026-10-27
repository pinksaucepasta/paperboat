//go:build darwin || linux

package hostruntimecmd

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
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

// degradedSteps names what the daemon reported failing, e.g. local machine access, for the rollback message.
func degradedSteps(codes []string) string {
	if len(codes) == 0 {
		return "no health item reported"
	}
	return strings.ReplaceAll(strings.Join(codes, ", "), "_", " ")
}

func restartAndAwaitDaemon(ctx context.Context, participants updated.UnixParticipants, version string, baseline updated.UnixParticipantProbe) error {
	if err := participants.Restart(ctx); err != nil {
		return errors.Join(errors.New("the user's Paperboat daemon could not be restarted on the new version"), err)
	}
	readyCtx, cancel := context.WithTimeout(ctx, suppliedUpdateReadyTimeout)
	defer cancel()
	requireInventory := baseline.Machines > 0 || baseline.State == "ready"
	var last error
	wait := updated.ParticipantNotReady
	delay := 250 * time.Millisecond
	for {
		probe, err := participants.Probe(readyCtx)
		if err == nil {
			// Only a daemon waiting for the control plane (a slow server) is waited for; an update is
			// never committed on that state, and any other degradation fails now (Lane F's rule, shared
			// with signed updates).
			if wait, last = updated.ClassifyParticipant(probe, version, baseline, requireInventory); wait == updated.ParticipantReadyNow {
				return nil
			} else if wait == updated.ParticipantDegradedLocally {
				return errors.Join(fmt.Errorf("the user's Paperboat daemon came up degraded on the new version (%s)", degradedSteps(probe.Health)), last)
			}
		} else {
			wait, last = updated.ParticipantNotReady, err
		}
		select {
		case <-readyCtx.Done():
			if wait == updated.ParticipantWaitingForControlPlane {
				return errors.Join(updated.ErrParticipantControlPlaneTimeout, last, readyCtx.Err())
			}
			return errors.Join(errors.New("the user's Paperboat daemon did not become ready on the new version"), last, readyCtx.Err())
		case <-time.After(delay):
		}
		delay = min(2*delay, 2*time.Second)
	}
}
