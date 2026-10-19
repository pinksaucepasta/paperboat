package statusbar

import (
	"fmt"
	"strings"
	"time"
)

func (b *Bar) SetIdentity(project, session string) {
	b.mu.Lock()
	b.project = safeLabel(project)
	b.session = safeLabel(session)
	b.updateTitleLocked()
	b.drawLocked()
	b.mu.Unlock()
}

// SetViewportChanged installs the live PTY resize callback after the remote
// connection has been established.
func (b *Bar) SetViewportChanged(callback func(cols, rows uint16)) {
	b.mu.Lock()
	b.viewportChanged = callback
	b.mu.Unlock()
}

func (b *Bar) updateTitleLocked() {
	if !b.terminalTitle || b.closed {
		return
	}
	if !b.titlePushed {
		_, _ = fmt.Fprint(b.serializedOut, "\x1b[22;0t")
		b.titlePushed = true
	}
	project, session, _ := b.identityLocked()
	if b.privacy {
		project, session = "private", "session"
	}
	_, _ = fmt.Fprintf(b.serializedOut, "\x1b]0;Paperboat - %s / %s\x07", project, session)
}

func (b *Bar) restoreTitleLocked() {
	if b.titlePushed {
		_, _ = fmt.Fprint(b.serializedOut, "\x1b[23;0t")
		b.titlePushed = false
	}
}

func (b *Bar) SetConnection(state string) {
	b.mu.Lock()
	b.connection = safeLabel(state)
	b.drawLocked()
	b.mu.Unlock()
}

// SetTransport records the transport used by the active terminal connection.
// It is rendered as the final d/q/w marker independently of configurable
// widgets: direct, relay QUIC, and relay WSS respectively. Unknown or empty
// values hide the marker until the next selection is observed.
func (b *Bar) SetTransport(transport string) {
	b.mu.Lock()
	switch strings.ToLower(strings.TrimSpace(transport)) {
	case "direct", "d":
		b.transport = "d"
	case "relay", "quic", "q":
		b.transport = "q"
	case "wss", "w":
		b.transport = "w"
	default:
		b.transport = ""
	}
	b.drawLocked()
	b.mu.Unlock()
}

// SetDebugVersions exposes the exact local CLI and remote runtime builds for
// this terminal connection. It is separate from configurable widgets so the
// per-session --debug flag cannot silently hide it through saved layout state.
func (b *Bar) SetDebugVersions(local, remote string) {
	b.mu.Lock()
	b.localVersion = safeLabel(local)
	b.remoteVersion = safeLabel(remote)
	b.drawLocked()
	b.mu.Unlock()
}

// SetUsage updates the account-level values exposed by the credits and
// storage widgets. Values are server-authoritative and already display-safe.
func (b *Bar) SetUsage(credits, storage string) {
	b.mu.Lock()
	b.credits = safeLabel(credits)
	b.storage = safeLabel(storage)
	b.drawLocked()
	b.mu.Unlock()
}

// SetConfigSync updates the persistent config_sync widget without exposing
// server error details or synced paths.
func (b *Bar) SetConfigSync(state string) {
	b.mu.Lock()
	b.configSync = safeLabel(state)
	b.drawLocked()
	b.mu.Unlock()
}

// Text returns the current printable status text. It is useful for terminal
// integrations that need to observe status without inspecting ANSI output.
func (b *Bar) Text() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.textLocked()
}

// Render returns the current full-width bar text without writing terminal
// control sequences. It is useful to integrations that need to observe the
// configured widget layout.
func (b *Bar) Render(width int) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.layoutLocked(width)
}

// Notice temporarily replaces the identity line. Failures take precedence so
// they remain visible until the responsible lifecycle reports recovery.
func (b *Bar) Notice(message string) {
	b.mu.Lock()
	b.notice = safeLabel(message)
	b.noticeUntil = time.Now().Add(b.noticeDuration)
	b.loading = false
	b.stopSpinnerLocked()
	b.resetTimerLocked()
	b.drawLocked()
	b.mu.Unlock()
}

// Loading temporarily replaces the center region with an ASCII spinner and
// message. It is used only for work that is actively progressing.
func (b *Bar) Loading(message string) {
	b.mu.Lock()
	b.notice = safeLabel(message)
	b.noticeUntil = time.Now().Add(b.noticeDuration)
	b.loading = true
	b.resetTimerLocked()
	b.startSpinnerLocked()
	b.drawLocked()
	b.mu.Unlock()
}

// LoadingPersistent shows active work until another notice or failure replaces it.
func (b *Bar) LoadingPersistent(message string) {
	b.mu.Lock()
	b.notice = safeLabel(message)
	b.noticeUntil = time.Time{}
	b.loading = true
	if b.timer != nil {
		b.timer.Stop()
		b.timer = nil
	}
	b.startSpinnerLocked()
	b.drawLocked()
	b.mu.Unlock()
}

func (b *Bar) Failure(message string) {
	b.FailureFor("general", message)
}

// FailureFor keeps an error visible until the same lifecycle source recovers.
func (b *Bar) FailureFor(source, message string) {
	b.mu.Lock()
	b.failureOrder++
	b.failures[safeLabel(source)] = failure{message: safeLabel(message), order: b.failureOrder}
	b.loading = false
	b.stopSpinnerLocked()
	b.drawLocked()
	b.mu.Unlock()
}

func (b *Bar) RecoverFailure() {
	b.RecoverFailureFor("general")
}

func (b *Bar) RecoverFailureFor(source string) {
	b.mu.Lock()
	delete(b.failures, safeLabel(source))
	b.drawLocked()
	b.mu.Unlock()
}

func (b *Bar) resetTimerLocked() {
	if b.timer != nil {
		b.timer.Stop()
	}
	delay := time.Until(b.noticeUntil)
	if delay <= 0 {
		delay = time.Millisecond
	}
	b.timer = time.AfterFunc(delay, func() {
		b.mu.Lock()
		if !b.noticeUntil.After(time.Now()) {
			b.notice = ""
			b.loading = false
			b.stopSpinnerLocked()
			b.drawLocked()
		}
		b.mu.Unlock()
	})
}

func (b *Bar) startSpinnerLocked() {
	if !b.loading || b.closed || b.spinnerTimer != nil {
		return
	}
	b.spinnerTimer = time.AfterFunc(120*time.Millisecond, func() {
		b.mu.Lock()
		b.spinnerTimer = nil
		if b.loading && !b.closed && b.noticeActiveLocked() {
			b.spinner = (b.spinner + 1) % 4
			b.drawLocked()
			b.startSpinnerLocked()
		}
		b.mu.Unlock()
	})
}

func (b *Bar) noticeActiveLocked() bool {
	return b.notice != "" && (b.noticeUntil.IsZero() || b.noticeUntil.After(time.Now()))
}

func (b *Bar) stopSpinnerLocked() {
	if b.spinnerTimer != nil {
		b.spinnerTimer.Stop()
		b.spinnerTimer = nil
	}
}
