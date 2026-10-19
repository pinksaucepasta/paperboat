package statusbar

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/ansi/parser"
)

// ResetRemoteState discards parser state that cannot safely cross a transport
// discontinuity. The reattached application will redraw after its PTY resize.
func (b *Bar) ResetRemoteState() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.parser = ansi.NewParser()
	b.ansiState = byte(parser.GroundState)
	b.appCursorSaved = false
	b.appInverse = false
	b.appSGR = nil
	b.appSGROverflow = false
	// The local terminal survives a transport reconnect. Preserve the last
	// complete alternate-screen state instead of guessing from a new stream.
	b.synchronized = false
	b.scrollDirty = true
	b.redrawPending = true
}

func (b *Bar) consumeANSI(data []byte) (invalidated bool) {
	if b.ansiState == byte(parser.GroundState) && !bytes.ContainsRune(data, '\x1b') {
		return false
	}
	for len(data) > 0 {
		_, _, n, state := ansi.DecodeSequence(data, b.ansiState, b.parser)
		if n <= 0 {
			return invalidated
		}
		if b.resetsScrollRegion(data[:n]) {
			b.scrollDirty = true
			invalidated = true
			if b.hardResetRestoresViewport(data[:n]) {
				b.alternate = false
				if b.suspended {
					b.suspended = false
					b.notifyViewportLocked()
				}
			}
		}
		if b.erasesStatusRow(data[:n]) {
			invalidated = true
		}
		wasSynchronized := b.synchronized
		b.trackRemoteCursorSave(data[:n])
		b.trackRemoteSGR(data[:n])
		b.trackSynchronizedOutput(data[:n])
		if b.trackAlternateScreen(data[:n]) {
			invalidated = true
		}
		if wasSynchronized && !b.synchronized {
			invalidated = true
		}
		b.ansiState = state
		data = data[n:]
	}
	return invalidated
}

func (b *Bar) hardResetRestoresViewport(sequence []byte) bool {
	return len(sequence) == 2 && sequence[0] == '\x1b' && sequence[1] == 'c'
}

func (b *Bar) trackAlternateScreen(sequence []byte) bool {
	if b.fullscreen != FullscreenHide || len(sequence) < 6 || sequence[0] != '\x1b' {
		return false
	}
	raw := string(sequence)
	if !(strings.Contains(raw, "?1049") || strings.Contains(raw, "?1047") || strings.Contains(raw, "?47")) {
		return false
	}
	entering := sequence[len(sequence)-1] == 'h'
	leaving := sequence[len(sequence)-1] == 'l'
	if (!entering && !leaving) || b.alternate == entering {
		return false
	}
	b.alternate = entering
	if entering {
		_, _ = fmt.Fprint(b.serializedOut, "\x1b[r")
		b.scrollRows = 0
		b.scrollDirty = true
		b.suspended = true
	} else {
		b.suspended = false
		b.scrollDirty = true
	}
	b.notifyViewportLocked()
	return true
}

func (b *Bar) trackSynchronizedOutput(sequence []byte) {
	switch string(sequence) {
	case "\x1b[?2026h":
		b.synchronized = true
	case "\x1b[?2026l":
		b.synchronized = false
	}
}

func (b *Bar) trackRemoteCursorSave(sequence []byte) {
	switch string(sequence) {
	case "\x1b[s":
		b.appCursorSaved = true
	case "\x1b[u":
		b.appCursorSaved = false
	}
}

func (b *Bar) trackRemoteSGR(sequence []byte) {
	if len(sequence) < 3 || !strings.HasPrefix(string(sequence), "\x1b[") || sequence[len(sequence)-1] != 'm' {
		return
	}
	params := strings.TrimSuffix(strings.TrimPrefix(string(sequence), "\x1b["), "m")
	if params == "" || params == "0" {
		b.appSGR = append(b.appSGR[:0], sequence...)
		b.appSGROverflow = false
	} else if len(b.appSGR)+len(sequence) <= 4096 {
		b.appSGR = append(b.appSGR, sequence...)
	} else {
		b.appSGROverflow = true
	}
	if params == "" {
		b.appInverse = false
		return
	}
	for _, raw := range strings.Split(params, ";") {
		switch raw {
		case "0":
			b.appInverse = false
		case "7":
			b.appInverse = true
		case "27":
			b.appInverse = false
		}
	}
}

func (b *Bar) resetsScrollRegion(sequence []byte) bool {
	if len(sequence) == 0 {
		return false
	}
	// DECSTBM, RIS, and alternate-screen swaps can reset the physical scroll
	// margins. Reapply our local margin after the remote sequence is complete.
	if len(sequence) == 2 && sequence[0] == '\x1b' && sequence[1] == 'c' {
		return true
	}
	if !strings.HasPrefix(string(sequence), "\x1b[") {
		return false
	}
	last := sequence[len(sequence)-1]
	if last == 'r' {
		return true
	}
	return (last == 'h' || last == 'l') && bytes.Contains(sequence, []byte("?1049"))
}

func (b *Bar) erasesStatusRow(sequence []byte) bool {
	if len(sequence) < 3 || !bytes.HasPrefix(sequence, []byte("\x1b[")) {
		return false
	}
	// ED can erase outside the remote scroll region, including the status row.
	return sequence[len(sequence)-1] == 'J'
}
