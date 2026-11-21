//go:build darwin || linux || windows

package runtime

import (
	"context"
	"errors"
	"sync"
	"time"
)

// errInboundWedged ends a generation whose inbound handshakes keep timing out
// while none completes; supervise rebuilds the listener, socket and endpoints.
var errInboundWedged = errors.New("inbound peer handshakes keep timing out; rebuilding the native listener")

const (
	inboundWedgeFailures  = 5
	inboundWedgeWindow    = time.Minute
	inboundWedgeMaxWindow = 30 * time.Minute
)

// inboundWatchdog trips when inboundWedgeFailures handshake timeouts span the
// window with no accept in between. Prior art: tailscale wgengine/watchdog.go
// and magicsock Rebind+ReSTUN after socket errors (magicsock.go). Each trip
// without a later successful accept doubles the window, so a peer that can
// never finish a handshake cannot turn this into a rebuild loop.
type inboundWatchdog struct {
	mu       sync.Mutex
	now      func() time.Time
	trips    int // consecutive trips with no accept since; survives generations
	failures int
	first    time.Time
	last     time.Time
}

func (w *inboundWatchdog) window() time.Duration {
	return min(inboundWedgeWindow<<min(w.trips, 5), inboundWedgeMaxWindow)
}

// observe returns true once when the generation should be rebuilt, with the
// window the next trip needs.
func (w *inboundWatchdog) observe(kind string, err error) (bool, time.Duration) {
	w.mu.Lock()
	defer w.mu.Unlock()
	switch {
	case kind == "accepted":
		w.trips, w.failures = 0, 0
		return false, 0
	case kind != "accept_failed" || !errors.Is(err, context.DeadlineExceeded):
		return false, 0
	}
	now := time.Now()
	if w.now != nil {
		now = w.now()
	}
	// A gap of a full window means the failures are unrelated, not a wedge.
	if w.failures == 0 || now.Sub(w.last) >= inboundWedgeWindow {
		w.first, w.failures = now, 0
	}
	w.last = now
	w.failures++
	if w.failures < inboundWedgeFailures || now.Sub(w.first) < w.window() {
		return false, 0
	}
	w.trips++
	w.failures = 0
	return true, w.window()
}

// reset starts a new generation's count; the trip streak is kept.
func (w *inboundWatchdog) reset() {
	w.mu.Lock()
	w.failures = 0
	w.mu.Unlock()
}
