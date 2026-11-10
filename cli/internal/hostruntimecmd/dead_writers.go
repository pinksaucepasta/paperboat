package hostruntimecmd

import (
	"context"
	"errors"
	"time"
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

// errInstallerLive is an install lock held by a running installer: its journal is its own.
var errInstallerLive = errors.New("an installation is running")

// deadInstaller is one look for a Windows installation whose installer died after it stopped the services (victus
// 2026-11-10: `pb update` killed 8 s after the stop left no hostd and no daemon until `pb doctor --repair`). The
// Linux updater does this look itself (recoverDeadWriters); on Windows the installer stops the updater, so the
// carrier's service process, which every install leaves running, does it.
type deadInstaller struct {
	pending func() (bool, error)                        // the candidate record of a replacement is left
	tryLock func(context.Context) (func() error, error) // errInstallerLive when a running installer holds a lock; else the install lock, held
	recover func(context.Context) error                 // restore the previous installation, withdraw the candidate
}

// check reports whether it recovered something. A live installer holds the locks for its whole run and is left alone.
func (d deadInstaller) check(ctx context.Context) (bool, error) {
	if pending, err := d.pending(); err != nil || !pending {
		return false, err
	}
	unlock, err := d.tryLock(ctx)
	if errors.Is(err, errInstallerLive) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	// The installer may have finished between the look and the lock.
	if pending, err := d.pending(); err != nil || !pending {
		return false, errors.Join(err, unlock())
	}
	if err := d.recover(ctx); err != nil {
		return false, errors.Join(err, unlock())
	}
	return true, unlock()
}

// watch runs check at once and then on the dead-writer schedule until ctx ends: every poll, a failing recovery
// reported and retried with backoff, and after a recovery the next look starts over.
func (d deadInstaller) watch(ctx context.Context, poll, maxWait time.Duration, report func(error)) {
	if _, err := d.check(ctx); err != nil && ctx.Err() == nil {
		report(err) // the first look does not wait: the service may have just started at boot
	}
	for ctx.Err() == nil {
		_ = superviseDeadWriters(ctx, poll, maxWait, d.check, report, func(ctx context.Context) error {
			<-ctx.Done()
			return nil
		})
	}
}
