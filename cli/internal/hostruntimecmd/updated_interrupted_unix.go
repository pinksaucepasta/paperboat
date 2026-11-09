//go:build darwin || linux

package hostruntimecmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostinstall"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
)

// deadWriterPoll is how often a running updater looks for state a dead writer left. Each look is a few stats
// and non-blocking lock attempts; a failing recovery is retried after twice the wait, up to deadWriterMaxWait.
const (
	deadWriterPoll    = 30 * time.Second
	deadWriterMaxWait = 10 * time.Minute
)

var errDeadWriterRecovered = errors.New("state a dead writer left was recovered; the updater restarts to read the restored installation")

// superviseDeadWriters runs the updater and, beside it, recovers what a killed installer, activation helper or
// update left (pbvm1 F14-03: the killed client left its journals and the new machine guard, and nothing recovered
// them because recovery ran only when the updater started). check reports whether it changed state; the updater
// then stops with errDeadWriterRecovered, so its supervisor starts it on the restored installation and the start
// path heals the guard. A failing check is reported and retried with backoff; the updater keeps serving.
func superviseDeadWriters(ctx context.Context, poll, maxWait time.Duration, check func(context.Context) (bool, error), report func(error), run func(context.Context) error) error {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	stopped := make(chan struct{}, 1)
	go func() {
		wait := poll
		for {
			select {
			case <-runCtx.Done():
				return
			case <-time.After(wait):
			}
			acted, err := check(runCtx)
			switch {
			case err != nil && runCtx.Err() == nil:
				report(err)
				wait = min(2*wait, maxWait)
			case acted:
				stopped <- struct{}{}
				cancel()
				return
			default:
				wait = poll
			}
		}
	}()
	runErr := run(runCtx)
	select {
	case <-stopped:
		return errors.Join(errDeadWriterRecovered, runErr)
	default:
		return runErr
	}
}

// recoverDeadWriters recovers, in this order, an interrupted install (hostinstall.RecoverInterrupted, with the
// updater's own service left for last) and the updater-owned state of a dead activation (dead is
// updated.Service.RecoverDeadWriters).
func recoverDeadWriters(uid int, stderr io.Writer, dead func(context.Context) (bool, error)) func(context.Context) (bool, error) {
	return func(ctx context.Context) (bool, error) {
		recovery, err := hostinstall.RecoverInterrupted(ctx, uid, service.UpdaterKind)
		if err != nil && !errors.Is(err, hostinstall.ErrRecoveryBusy) {
			return false, runtimeFailure("recovering the interrupted installation failed", err)
		}
		if err == nil && recovery.Undone {
			if recovery.Cleanup != nil {
				fmt.Fprintf(stderr, "paperboat-updated: the interrupted installation was undone, but part of what it staged stays: %v\n", recovery.Cleanup)
			}
			if recovery.FinishSelf != nil {
				if err := recovery.FinishSelf(ctx); err != nil {
					return false, runtimeFailure("returning the updater's own service to its state before the interrupted installation failed", err)
				}
			}
			return true, nil
		}
		return dead(ctx)
	}
}
