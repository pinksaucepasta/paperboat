//go:build linux || darwin

package hostinstall

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/hostdproto"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/updated"
)

type closedLock struct{}

func (closedLock) Close() error { return nil }

// While the activation lock is held (here: by the updater install-supplied just restarted, as it starts), starting
// an explicit update refuses at once, but that update's own commit and rollback steps wait for it (smoke 11-27
// linuxpool-3: 5 of 10 same-version pb update --from failed at commit-supplied with "hostd owner maintenance is busy
// ... the installed version is unchanged" after the services had been replaced).
func TestSuppliedUpdateCommitWaitsForABriefActivationLockHolder(t *testing.T) {
	previousTry, previousLock := tryLockNative, lockNative
	t.Cleanup(func() { tryLockNative, lockNative = previousTry, previousLock })
	tryLockNative = func(string) (io.Closer, error) { return nil, updated.ErrActivationPending }
	waited := 0
	lockNative = func(context.Context, string) (io.Closer, error) { waited++; return closedLock{}, nil }
	ctx := context.Background()
	if _, err := lockSuppliedNative(ctx, "/state", "install-supplied", true); !errors.Is(err, hostdproto.ErrMaintenanceBusy) || waited != 0 {
		t.Fatalf("starting an explicit update must refuse at once: err=%v waited=%d", err, waited)
	}
	for _, operation := range []string{"commit-supplied", "rollback-supplied"} {
		waited = 0
		if lock, err := lockSuppliedNative(ctx, "/state", operation, true); err != nil || lock == nil || waited != 1 {
			t.Fatalf("%s must wait for the lock: err=%v waited=%d", operation, err, waited)
		}
	}
	waited = 0
	if _, err := lockSuppliedNative(ctx, "/state", "install-supplied", false); err != nil || waited != 1 {
		t.Fatalf("an install that is not an explicit update waits: err=%v waited=%d", err, waited)
	}
}
