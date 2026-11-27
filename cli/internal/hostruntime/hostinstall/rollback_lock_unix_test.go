//go:build darwin || linux

package hostinstall

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/updated"
)

// rollback-supplied holds the activation lock for its whole run and then stops and restores the prior runtime
// (Stop, RestorePriorNativeRuntime -> Repair). Those took the lock again on a new descriptor of the same file, which
// flock refuses even inside one process: the rollback waited out its 45 s recovery context and never restored.
func TestNestedStepsOfAHeldActivationLockDoNotWaitForThemselves(t *testing.T) {
	paths, _ := heldActivation(t) // this process holds the lock, as SuppliedBinary's nativeLock does
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := lockInstallStateUnlessHeld(ctx, paths.updateState, false); !errors.Is(err, updated.ErrActivationPending) {
		t.Fatalf("re-locking a lock this process holds = %v, want it to wait and fail (the self-deadlock)", err)
	}
	ctx, cancel = context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	started := time.Now()
	lock, err := lockInstallStateUnlessHeld(ctx, paths.updateState, true)
	if err != nil || lock == nil || time.Since(started) > 100*time.Millisecond {
		t.Fatalf("held lock = %v %v after %s, want admission at once", lock, err, time.Since(started))
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	// Closing the borrowed lock must not release the holder's.
	if _, err := updated.TryLockUnixNativeInstall(paths.updateState); !errors.Is(err, updated.ErrActivationPending) {
		t.Fatalf("after the nested step, the holder's lock = %v, want still held", err)
	}
}
