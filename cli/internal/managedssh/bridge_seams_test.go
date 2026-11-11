package managedssh

import (
	"context"
	"net"
	"time"
)

// The overrides return what puts the previous value back; use them as `defer overrideX(...)()`.

func overrideDialSSHD(dial func(ctx context.Context, address string) (net.Conn, error)) func() {
	seams.mu.Lock()
	previous := seams.dial
	seams.dial = dial
	seams.mu.Unlock()
	return func() {
		seams.mu.Lock()
		seams.dial = previous
		seams.mu.Unlock()
	}
}

func overrideResetGrace(grace time.Duration) func() {
	seams.mu.Lock()
	previous := seams.grace
	seams.grace = grace
	seams.mu.Unlock()
	return func() {
		seams.mu.Lock()
		seams.grace = previous
		seams.mu.Unlock()
	}
}

func overrideCloseResets(resets []error) func() {
	seams.mu.Lock()
	previousList, previousSet := seams.resets, seams.setResets
	seams.resets, seams.setResets = resets, true
	seams.mu.Unlock()
	return func() {
		seams.mu.Lock()
		seams.resets, seams.setResets = previousList, previousSet
		seams.mu.Unlock()
	}
}
