package hostinstall

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"
)

// WindowsLateSSHWindow bounds how long after an installation the updater keeps trying to set up the managed SSH the
// installation skipped because OpenSSH could not be obtained in its budget (a winget download that failed or was slow).
const WindowsLateSSHWindow = 15 * time.Minute

const (
	windowsLateSSHFirstDelay = time.Minute
	windowsLateSSHMaxDelay   = 4 * time.Minute
)

// windowsLateSSH is the updater's retry state: one owner, the reconcile goroutine.
type windowsLateSSH struct {
	next         time.Time
	delay        time.Duration
	pendingUntil atomic.Pointer[time.Time]
}

// pending reports the live attempt or scheduled backoff to the updater status reader.
func (r *windowsLateSSH) pending(now time.Time) bool {
	until := r.pendingUntil.Load()
	return until != nil && !now.After(*until)
}

// errWindowsLateSSHSuperseded is prepare's answer when an update replaced the installed generation meanwhile: the
// next tick reads the new installation, with no backoff carried over.
var errWindowsLateSSHSuperseded = errors.New("installed generation replaced")

// complete runs prepare when the installed generation's SSH is missing, the installation is inside WindowsLateSSHWindow
// and the backoff allows; after a failed or skipped attempt the delay doubles up to windowsLateSSHMaxDelay. A failure
// is returned (and so reported) only when no attempt is left in the window: retries inside it are expected.
func (r *windowsLateSSH) complete(ctx context.Context, now, installedAt time.Time, present func(context.Context) bool, prepare func(context.Context) error) error {
	deadline := installedAt.Add(WindowsLateSSHWindow)
	if now.After(deadline) || present(ctx) {
		r.pendingUntil.Store(nil)
		return nil
	}
	if now.Before(r.next) {
		return nil
	}
	r.pendingUntil.Store(&deadline)
	err := prepare(ctx)
	if errors.Is(err, errWindowsLateSSHSuperseded) {
		r.delay, r.next = 0, time.Time{}
		r.pendingUntil.Store(nil)
		return nil
	}
	if err == nil && present(ctx) {
		r.delay, r.next = 0, time.Time{}
		r.pendingUntil.Store(nil)
		return nil
	}
	r.delay = min(max(2*r.delay, windowsLateSSHFirstDelay), windowsLateSSHMaxDelay)
	r.next = now.Add(r.delay)
	if r.next.Sub(installedAt) <= WindowsLateSSHWindow {
		return nil
	}
	r.pendingUntil.Store(nil)
	if err == nil {
		err = errors.New("managed SSH is still not prepared: OpenSSH could not be obtained")
	}
	return fmt.Errorf("set up managed SSH within %s of installation (run pb doctor --repair): %w", WindowsLateSSHWindow, err)
}
