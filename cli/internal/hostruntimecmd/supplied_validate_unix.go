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
func suppliedUpdateValidator(ctx context.Context, request hostinstall.Request) func(context.Context) error {
	layout, err := service.UserLayout(runtime.GOOS, request.UID)
	if err != nil {
		return nil
	}
	participants, err := newUnixUpdateParticipants(layout.Binary, request.UID, request.GID)
	if err != nil {
		return nil
	}
	baseline, baselineErr := participants.Probe(ctx)
	if baselineErr != nil || !baseline.Running {
		return nil
	}
	return func(ctx context.Context) error {
		return restartAndAwaitDaemon(ctx, participants, request.Source.Version, baseline)
	}
}

func restartAndAwaitDaemon(ctx context.Context, participants updated.UnixParticipants, version string, baseline updated.UnixParticipantProbe) error {
	if err := participants.Restart(ctx); err != nil {
		return errors.Join(errors.New("the user's Paperboat daemon could not be restarted on the new version"), err)
	}
	readyCtx, cancel := context.WithTimeout(ctx, suppliedUpdateReadyTimeout)
	defer cancel()
	requireInventory := baseline.Machines > 0 || baseline.State == "ready"
	var last error
	for {
		probe, err := participants.Probe(readyCtx)
		if err == nil {
			if last = updated.ParticipantReady(probe, version, baseline, requireInventory); last == nil {
				return nil
			}
		} else {
			last = err
		}
		select {
		case <-readyCtx.Done():
			return errors.Join(errors.New("the user's Paperboat daemon did not become ready on the new version"), last, readyCtx.Err())
		case <-time.After(250 * time.Millisecond):
		}
	}
}
