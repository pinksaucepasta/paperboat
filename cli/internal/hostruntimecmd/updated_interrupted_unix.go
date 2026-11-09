//go:build darwin || linux

package hostruntimecmd

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostinstall"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
)

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
