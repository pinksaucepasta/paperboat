package hostinstall

import (
	"context"
	"errors"
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
	next  time.Time
	delay time.Duration
}

// complete runs prepare when the installed generation's SSH is missing, the installation is inside WindowsLateSSHWindow
// and the backoff allows; after a failed or skipped attempt the delay doubles up to windowsLateSSHMaxDelay.
func (r *windowsLateSSH) complete(ctx context.Context, now, installedAt time.Time, present func(context.Context) bool, prepare func(context.Context) error) error {
	if now.Sub(installedAt) > WindowsLateSSHWindow || now.Before(r.next) || present(ctx) {
		return nil
	}
	err := prepare(ctx)
	if err == nil && present(ctx) {
		r.delay = 0
		return nil
	}
	r.delay = min(max(2*r.delay, windowsLateSSHFirstDelay), windowsLateSSHMaxDelay)
	r.next = now.Add(r.delay)
	if err == nil {
		err = errors.New("managed SSH is still not prepared: OpenSSH could not be obtained")
	}
	return err
}
