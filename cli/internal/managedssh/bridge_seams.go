package managedssh

import (
	"context"
	"net"
	"sync"
	"time"
)

// Three values of the bridge are fixed in production and replaced by tests: how the loopback sshd is dialed, how long a
// reset waits for the other direction, and which errors count as a reset. A bridge goroutine of an earlier test can still
// be running when the next test sets one, so every read and write goes through this lock (the race detector reported
// them as plain variables).
var seams struct {
	mu        sync.RWMutex
	dial      func(ctx context.Context, address string) (net.Conn, error)
	grace     time.Duration
	resets    []error
	setResets bool
}

const defaultResetGrace = 2 * time.Second

// dialSSHD opens the connection to the loopback sshd.
func dialSSHD(ctx context.Context, address string) (net.Conn, error) {
	seams.mu.RLock()
	dial := seams.dial
	seams.mu.RUnlock()
	if dial != nil {
		return dial(ctx, address)
	}
	return (&net.Dialer{}).DialContext(ctx, "tcp", address)
}

// resetGrace bounds how long a connection reset waits for the other direction to show a plain EOF.
func resetGrace() time.Duration {
	seams.mu.RLock()
	defer seams.mu.RUnlock()
	if seams.grace != 0 {
		return seams.grace
	}
	return defaultResetGrace
}

// resetErrors are the errors a connection reset may be: closeResets of the platform unless a test replaced them.
func resetErrors() []error {
	seams.mu.RLock()
	defer seams.mu.RUnlock()
	if seams.setResets {
		return seams.resets
	}
	return closeResets
}
