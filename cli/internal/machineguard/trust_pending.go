//go:build linux || darwin || windows

package machineguard

import (
	"context"
	"errors"
	"fmt"
)

// ErrTrustPending reports that the machine guard is installed, running and ready,
// but browser HTTPS trust could not be installed in the operating system yet, for
// example because the OS denied the approval prompt or a trust tool is missing.
// Retrying the installation after approving the trust request completes it.
var ErrTrustPending = errors.New("browser HTTPS trust is pending approval")

// trustPending classifies an operating-system trust failure as pending. Context
// cancellation is never pending: it aborts the installation like any other failure.
func trustPending(ctx context.Context, cause error) error {
	if cause == nil {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return fmt.Errorf("%w: %v", ErrTrustPending, cause)
}

func isTrustPending(err error) bool { return errors.Is(err, ErrTrustPending) }

// finishGuardInstall rolls back a failed guard installation. A pending trust result
// means the new guard is installed and ready, so its files and running state stay.
func finishGuardInstall(ctx context.Context, rollback *guardFileRollback, result *error, undo **Undo, stop, restore func(context.Context) error) {
	if *result == nil || isTrustPending(*result) {
		if undo == nil {
			rollback.cleanup()
			return
		}
		// Installed: the prior installation stays restorable until the whole update commits.
		*undo = &Undo{rollback: rollback, stop: stop, restore: restore}
		return
	}
	finishGuardInstallation(ctx, rollback, result, stop, restore)
}
