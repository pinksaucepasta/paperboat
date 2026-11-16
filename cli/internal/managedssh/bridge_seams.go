package managedssh

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/pinksaucepasta/paperboat/shared/diagnosticlog"
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
func dialSSHD(ctx context.Context, address string, source netip.Addr) (net.Conn, error) {
	seams.mu.RLock()
	dial := seams.dial
	seams.mu.RUnlock()
	if dial != nil {
		return dial(ctx, address)
	}
	if !source.Is4() || !strings.HasPrefix(address, "127.") {
		return (&net.Dialer{}).DialContext(ctx, "tcp", address)
	}
	connection, err := (&net.Dialer{LocalAddr: &net.TCPAddr{IP: source.AsSlice()}}).DialContext(ctx, "tcp", address)
	if errors.Is(err, syscall.EADDRNOTAVAIL) {
		// macOS routes only the 127/8 addresses its machine guard aliased: without them the user shares 127.0.0.1's
		// penalties instead of losing SSH, said once here and by pb doctor (managed_ssh_sources).
		if !sourceMissingLogged.Swap(true) {
			diagnosticlog.TryWarn("managed SSH connects to sshd from 127.0.0.1: the per-user source addresses the machine guard adds are missing, so one user's failed logins can block every user's managed SSH; run pb install again, then pb doctor", "source", source.String())
		}
		return (&net.Dialer{}).DialContext(ctx, "tcp", address)
	}
	if err == nil {
		sourceMissingLogged.Store(false)
	}
	return connection, err
}

// sourceMissingLogged: the missing source addresses were logged and have not been bindable since.
var sourceMissingLogged atomic.Bool

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
