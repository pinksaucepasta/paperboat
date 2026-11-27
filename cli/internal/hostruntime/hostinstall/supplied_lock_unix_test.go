//go:build linux || darwin

package hostinstall

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

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

// A signed activation queued after a bound install-supplied committed holds the lock for its whole health check:
// commit-supplied says once what it waits for and waits at most one activation, not 15 min in silence.
func TestSuppliedUpdateCommitSaysWhatItWaitsForAndIsBounded(t *testing.T) {
	previousTry, previousLock, previousOutput := tryLockNative, lockNative, suppliedWaitOutput
	t.Cleanup(func() { tryLockNative, lockNative, suppliedWaitOutput = previousTry, previousLock, previousOutput })
	tryLockNative = func(string) (io.Closer, error) { return nil, updated.ErrActivationPending }
	var deadline time.Time
	lockNative = func(ctx context.Context, _ string) (io.Closer, error) {
		deadline, _ = ctx.Deadline()
		return closedLock{}, nil
	}
	for _, operation := range []string{"commit-supplied", "rollback-supplied"} {
		var output bytes.Buffer
		suppliedWaitOutput = &output
		deadline = time.Time{}
		ctx, want := context.Background(), "at most 15 min)"
		if operation == "rollback-supplied" {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, 45*time.Second) // its caller's recovery bound
			defer cancel()
			want = "at most 1 min)"
		}
		if _, err := lockSuppliedNative(ctx, t.TempDir(), operation, true); err != nil {
			t.Fatal(err)
		}
		if got := output.String(); strings.Count(got, "Waiting for another Paperboat update that is finishing") != 1 || !strings.Contains(got, want) {
			t.Fatalf("%s printed %q", operation, got)
		}
		if deadline.IsZero() || time.Until(deadline) > suppliedCommitWait {
			t.Fatalf("%s waits unbounded or past one activation: deadline %v", operation, deadline)
		}
	}
	tryLockNative = func(string) (io.Closer, error) { return closedLock{}, nil }
	var output bytes.Buffer
	suppliedWaitOutput = &output
	if _, err := lockSuppliedNative(context.Background(), t.TempDir(), "commit-supplied", true); err != nil || output.Len() != 0 {
		t.Fatalf("a free lock prints nothing: err=%v output=%q", err, output.String())
	}
}
