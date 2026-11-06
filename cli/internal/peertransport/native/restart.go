package native

import "time"

const (
	// restartMarkTTL is how long a peer's clean close stays a restart mark. A dial hours later has nothing to do with
	// that close: retiring the idle WireGuard client for it would only cost a fresh handshake.
	restartMarkTTL = 30 * time.Second
	// restartQuickWindow is how long dials to a restarted peer keep the short handshake wait, counted from the
	// handshake that first used it (not from the mark: PrepareRegional, the carrier dial and other dials come first).
	// A failed short handshake extends it, up to restartQuickMax from the start, so it lasts until one succeeds
	// without starving a path that became slow.
	restartQuickWindow = 3 * time.Second
	restartQuickMax    = 15 * time.Second
	// handshakeTookTTL is how long a measured handshake predicts the next one.
	handshakeTookTTL = 10 * time.Minute
)

type restartWindow struct{ start, until time.Time } // zero start: the mark is consumed, no handshake has used it yet

type measuredHandshake struct {
	took time.Duration
	at   time.Time
}

// restartTracker is the owner's per-peer memory of restarts. The owner's mutex guards it.
type restartTracker struct {
	marks   map[string]time.Time // peer -> when it closed a dialed session on purpose
	windows map[string]*restartWindow
	took    map[string]measuredHandshake
}

func (r *restartTracker) markClosed(peer string, now time.Time) {
	if r.marks == nil {
		r.marks = make(map[string]time.Time)
	}
	r.marks[peer] = now
}

// consume takes the peer's restart mark and, when it is still fresh, opens its (not yet started) window.
func (r *restartTracker) consume(peer string, now time.Time) bool {
	at, ok := r.marks[peer]
	delete(r.marks, peer)
	if !ok || now.Sub(at) >= restartMarkTTL {
		return false
	}
	if r.windows == nil {
		r.windows = make(map[string]*restartWindow)
	}
	r.windows[peer] = &restartWindow{}
	return true
}

// wait is the handshake timeout of a handshake starting now (0: the normal timeout). The first call after consume
// starts the window.
func (r *restartTracker) wait(peer string, local bool, now time.Time) time.Duration {
	w := r.windows[peer]
	if w != nil {
		if w.start.IsZero() {
			w.start, w.until = now, now.Add(restartQuickWindow)
		} else if now.After(w.until) {
			delete(r.windows, peer)
			w = nil
		}
	}
	if w == nil && !local {
		return 0
	}
	m, known := r.took[peer]
	if known && now.Sub(m.at) >= handshakeTookTTL {
		known = false
	}
	if !known && !local {
		return 0 // nothing recent says how long this path takes: keep the normal timeout
	}
	return restartHandshake(m.took) // a local peer with no measurement gets the floor
}

// failed extends an open window after a handshake that used it did not complete.
func (r *restartTracker) failed(peer string, now time.Time) {
	if w := r.windows[peer]; w != nil && !w.start.IsZero() {
		w.until = now.Add(restartQuickWindow)
		if limit := w.start.Add(restartQuickMax); w.until.After(limit) {
			w.until = limit
		}
	}
}

func (r *restartTracker) succeeded(peer string, took time.Duration, now time.Time) {
	delete(r.windows, peer)
	if r.took == nil {
		r.took = make(map[string]measuredHandshake)
	}
	r.took[peer] = measuredHandshake{took, now}
}
