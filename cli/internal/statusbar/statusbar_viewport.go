package statusbar

import (
	"fmt"

	"github.com/charmbracelet/x/ansi/parser"
)

// RemoteSize returns the physical terminal size minus the local status row
// when the bar is currently active. It is safe to call for initial attaches
// and every SIGWINCH.
func (b *Bar) RemoteSize() (cols, rows uint16) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return 0, 0
	}
	wasEnabled, wasRows := b.enabled, b.lastRows
	w, h := b.refreshViewportLocked()
	if w <= 0 || h <= 0 {
		return 0, 0
	}
	if b.activeLocked() && (!wasEnabled || wasRows != h) {
		b.drawLocked()
	}
	if b.activeLocked() {
		h--
	}
	return clamp(w, 1000), clamp(h, 500)
}

// PrepareRemoteViewport positions the local cursor inside the reserved
// scrolling region before the first remote byte arrives. It is intentionally
// called only after an attach succeeds; connection-progress rendering should
// leave the user's existing shell cursor alone.
func (b *Bar) PrepareRemoteViewport() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	_, h := b.refreshViewportLocked()
	if !b.activeLocked() || h < 2 {
		return
	}
	b.ensureScrollRegionLocked(h - 1)
	_, _ = fmt.Fprintf(b.serializedOut, "\x1b[%d;1H", h-1)
}

// ClearRemoteViewport starts an attached session on a clean local screen while
// preserving the reserved status row.
func (b *Bar) ClearRemoteViewport() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	_, _ = fmt.Fprint(b.serializedOut, "\x1b[2J\x1b[H")
	_, h := b.refreshViewportLocked()
	if !b.activeLocked() || h < 2 {
		return
	}
	b.ensureScrollRegionLocked(h - 1)
	b.drawLocked()
	_, _ = fmt.Fprintf(b.serializedOut, "\x1b[%d;1H", h-1)
}

// ClearForExit restores the full viewport and leaves the host terminal at a
// clean home position after the remote shell exits normally.
func (b *Bar) ClearForExit() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	if b.timer != nil {
		b.timer.Stop()
		b.timer = nil
	}
	b.stopSpinnerLocked()
	b.scrollRows = 0
	b.scrollDirty = true
	b.enabled = false
	b.closed = true
	_, _ = fmt.Fprint(b.serializedOut, "\x1b[r\x1b[2J\x1b[H")
	b.restoreTitleLocked()
}

func clamp(value, max int) uint16 {
	if value <= 0 {
		return 0
	}
	if value > max {
		value = max
	}
	return uint16(value)
}

func (b *Bar) notifyViewportLocked() {
	if b.viewportChanged == nil {
		return
	}
	w, h, err := b.getSize(b.outputFD)
	if err != nil || w <= 0 || h <= 0 {
		return
	}
	if b.activeLocked() {
		h--
	}
	b.viewportChanged(clamp(w, 1000), clamp(h, 500))
}

func (b *Bar) refreshViewportLocked() (int, int) {
	if !b.eligible {
		b.enabled = false
		w, h, err := b.getSize(b.outputFD)
		if err != nil {
			return 0, 0
		}
		return w, h
	}
	w, h, err := b.getSize(b.outputFD)
	if err != nil || w < minimumWidth || h < 2 {
		if b.enabled {
			b.clearLocked()
		}
		b.enabled = false
		return w, h
	}
	b.enabled = true
	b.lastRows = h
	return w, h
}

func (b *Bar) drawLocked() {
	b.redrawPending = true
	select {
	case b.redraw <- struct{}{}:
	default:
	}
}

func (b *Bar) drawNowLocked() {
	if b.closed {
		return
	}
	w, h := b.refreshViewportLocked()
	if !b.activeLocked() || b.ansiState != byte(parser.GroundState) || b.appCursorSaved || b.synchronized {
		b.redrawPending = true
		return
	}
	if w <= 0 || h < 2 {
		b.redrawPending = true
		return
	}
	text := b.layoutLocked(w)
	b.ensureScrollRegionLocked(h - 1)
	style, restore := b.barStyleLocked()
	_, _ = fmt.Fprintf(b.serializedOut, "\x1b[s\x1b[%d;1H\x1b[2K%s%s%s\x1b[u", h, style, text, restore)
	b.redrawPending = false
}

func (b *Bar) flushPendingRedrawLocked() {
	if !b.redrawPending || b.ansiState != byte(parser.GroundState) || b.appCursorSaved || b.synchronized {
		return
	}
	b.closed = false
	b.drawNowLocked()
	b.closed = true
}

func (b *Bar) ensureScrollRegionLocked(rows int) {
	if rows < 1 || (!b.scrollDirty && b.scrollRows == rows) {
		return
	}
	_, _ = fmt.Fprintf(b.serializedOut, "\x1b[s\x1b[1;%dr\x1b[u", rows)
	b.scrollRows = rows
	b.scrollDirty = false
}
