package native

import (
	"context"
	"errors"
	"sync"
	"time"
)

const (
	inboundWedgeFailures  = 5
	inboundWedgeWindow    = time.Minute
	inboundWedgeMaxWindow = 30 * time.Minute
)

// InboundWatchdog says when a listener's inbound handshakes are wedged:
// inboundWedgeFailures handshake timeouts span the window, with no accept and
// no gap of a full window between them. Prior art: tailscale
// wgengine/watchdog.go and magicsock Rebind+ReSTUN after socket errors. Each
// trip without a later accept doubles the window (capped), so a peer that can
// never finish a handshake cannot turn rebuilds into a loop.
type InboundWatchdog struct {
	mu       sync.Mutex
	Now      func() time.Time // nil: time.Now
	trips    int              // consecutive trips with no accept since; survives Reset
	wedged   bool             // tripped and no accept since
	rebuild  bool             // a rebuild was committed: Admit refuses until Reset
	failures int
	first    time.Time
	last     time.Time
}

func (w *InboundWatchdog) windowLocked() time.Duration {
	return min(inboundWedgeWindow<<min(w.trips, 5), inboundWedgeMaxWindow)
}

// Observe returns true once per trip, with the window the next trip needs.
func (w *InboundWatchdog) Observe(event Event) (bool, time.Duration) {
	w.mu.Lock()
	defer w.mu.Unlock()
	switch {
	case event.Kind != "accept_failed" || !errors.Is(event.Err, context.DeadlineExceeded):
		return false, 0
	}
	now := time.Now()
	if w.Now != nil {
		now = w.Now()
	}
	if w.failures == 0 || now.Sub(w.last) >= inboundWedgeWindow {
		w.first, w.failures = now, 0
	}
	w.last = now
	w.failures++
	if w.failures < inboundWedgeFailures || now.Sub(w.first) < w.windowLocked() {
		return false, 0
	}
	w.trips++
	w.failures, w.wedged = 0, true
	return true, w.windowLocked()
}

// Admit decides, under the same lock as CommitRebuild, whether a completed
// inbound handshake may become a session: either the accept wins and cancels
// the trip, or the committed rebuild wins and the accept is refused before it
// carries any traffic (the peer redials the rebuilt listener). Never both.
func (w *InboundWatchdog) Admit() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.rebuild {
		return false
	}
	w.trips, w.failures, w.wedged = 0, 0, false
	return true
}

// CommitRebuild reports whether a queued rebuild is still needed (a trip with
// no accept since) and, if so, commits to it until Reset.
func (w *InboundWatchdog) CommitRebuild() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.rebuild = w.wedged
	return w.wedged
}

// Reset starts a new listener's count; the trip streak is kept.
func (w *InboundWatchdog) Reset() {
	w.mu.Lock()
	w.failures, w.wedged, w.rebuild = 0, false, false
	w.mu.Unlock()
}
