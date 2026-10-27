package session

import (
	"sync"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/pty"
)

// sizeWindowRetention is how long a detached window keeps its activity, so a reconnect (the connection drops and the
// same attachment id comes back) returns to the control it had.
const sizeWindowRetention = 10 * time.Minute

// windowSizes decides one session's PTY size when several windows are attached (F3-17, Lane K terminal plan item 4).
//
// The rule is tmux's `window-size latest` (~/ref/tmux/options-table.c, resize.c): the PTY has the size of the controller
// window that was last active. Activity is typing and focus; attaching and reconnecting do not count, so a reconnect
// never takes control. A window is its attachment id, which a client keeps across its reconnects. Viewers never take
// part: they neither resize nor count.
//
// With no activity yet, the earliest attached controller sets the size (the session's first attachment).
// It is safe for concurrent use; input touches it from the hot path without any session lock.
type windowSizes struct {
	mu      sync.Mutex
	windows map[string]*sizeWindow
	next    uint64 // attach order, the tie-break and the "no activity yet" order
}

type sizeWindow struct {
	attached   bool
	detachedAt time.Time
	size       pty.Dimensions // the window's last reported size
	order      uint64         // when the window first attached
	active     time.Time      // host clock of its last typing or focus; zero until then
}

func newWindowSizes() *windowSizes { return &windowSizes{windows: make(map[string]*sizeWindow)} }

// attach registers a controller attachment with the size it reports. It never counts as activity: a window that was
// here before keeps its activity.
func (w *windowSizes) attach(id string, size pty.Dimensions, now time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for other, win := range w.windows {
		if !win.attached && now.Sub(win.detachedAt) > sizeWindowRetention {
			delete(w.windows, other)
		}
	}
	win := w.windows[id]
	if win == nil {
		w.next++
		win = &sizeWindow{order: w.next}
		w.windows[id] = win
	}
	win.attached = true
	if size.Columns != 0 && size.Rows != 0 {
		win.size = size
	}
}

// detach stops a window controlling; it keeps its activity for a reconnect.
func (w *windowSizes) detach(id string, now time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if win := w.windows[id]; win != nil {
		win.attached, win.detachedAt = false, now
	}
}

// report stores the size a window now has. It applies only if that window controls.
func (w *windowSizes) report(id string, size pty.Dimensions) {
	if size.Columns == 0 || size.Rows == 0 {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if win := w.windows[id]; win != nil && win.attached {
		win.size = size
	}
}

// touch records activity (typing or focus) at the host's clock and reports whether the controller changed because of
// it. An attachment that is not an attached controller (a viewer, an unknown id) is ignored.
func (w *windowSizes) touch(id string, at time.Time) (changed bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	win := w.windows[id]
	if win == nil || !win.attached || !at.After(win.active) {
		return false
	}
	pacer, _ := w.rankLocked(false)
	sizer, _ := w.rankLocked(true)
	win.active = at
	nextPacer, _ := w.rankLocked(false)
	nextSizer, _ := w.rankLocked(true)
	return pacer != nextPacer || sizer != nextSizer
}

// controller returns the attached window that paces output (whether or not its size is known) and the size of the
// latest active window that reported one; ok is false when none did, so the PTY keeps its size.
func (w *windowSizes) controller() (id string, size pty.Dimensions, ok bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	id, _ = w.rankLocked(false)
	sizer, best := w.rankLocked(true)
	if sizer == "" {
		return id, pty.Dimensions{}, false
	}
	return id, best.size, true
}

// rankLocked picks the latest active attached window; sized limits it to windows that reported a size. Pacing follows
// every window (an unsized one still paces a flood, N6); the PTY size only follows a window that has one.
func (w *windowSizes) rankLocked(sized bool) (id string, best *sizeWindow) {
	for candidate, win := range w.windows {
		if !win.attached || sized && (win.size.Columns == 0 || win.size.Rows == 0) {
			continue
		}
		if best == nil || latest(win, candidate, best, id) {
			best, id = win, candidate
		}
	}
	return id, best
}

// latest reports whether window a (id aid) ranks above window b (id bid): a later activity, else, with no activity
// on either, the earlier attachment; equal activity falls back to the lower id so the answer never flaps.
func latest(a *sizeWindow, aid string, b *sizeWindow, bid string) bool {
	switch {
	case a.active.After(b.active):
		return true
	case b.active.After(a.active):
		return false
	case a.active.IsZero() && a.order != b.order:
		return a.order < b.order
	}
	return aid < bid
}
