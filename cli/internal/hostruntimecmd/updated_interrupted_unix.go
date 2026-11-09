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

// interruptedInstallPoll is how often a running updater looks for an install whose process died. The look is one
// stat of the install journal unless a journal exists.
const interruptedInstallPoll = 10 * time.Second

var errInterruptedInstallUndone = errors.New("an interrupted installation was undone; the updater restarts to read the restored installation")

// superviseInterruptedInstall runs the updater and, beside it, undoes an install that was killed while the updater
// kept running (pbvm1 F14-03: the killed client left its journals and the new machine guard, and nothing recovered
// them because recovery ran only when the updater started). When check reports an undone install the updater
// stops with errInterruptedInstallUndone, so its supervisor starts it again on the restored installation and the
// start path heals the guard; a failing check stops it too, and the start path then retries with its own backoff.
func superviseInterruptedInstall(ctx context.Context, poll time.Duration, check func(context.Context) (bool, error), run func(context.Context) error) error {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	stopped := make(chan error, 1)
	go func() {
		ticker := time.NewTicker(poll)
		defer ticker.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-ticker.C:
				undone, err := check(runCtx)
				if err == nil && !undone {
					continue
				}
				if err == nil {
					err = errInterruptedInstallUndone
				}
				stopped <- err
				cancel()
				return
			}
		}
	}()
	runErr := run(runCtx)
	select {
	case err := <-stopped:
		return errors.Join(err, runErr)
	default:
		return runErr
	}
}

// recoverWhileRunning undoes the interrupted install the updater's own service is part of: its finishSelf is
// run last, as at the updater's start.
func recoverWhileRunning(uid int, stderr io.Writer) func(context.Context) (bool, error) {
	return func(ctx context.Context) (bool, error) {
		recovery, err := hostinstall.RecoverInterrupted(ctx, uid, service.UpdaterKind)
		if errors.Is(err, hostinstall.ErrRecoveryBusy) || err == nil && !recovery.Undone {
			return false, nil // another recoverer is at it, or nothing is pending, or its installer still runs
		}
		if err != nil {
			return false, runtimeFailure("recovering the interrupted installation failed", err)
		}
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
}
