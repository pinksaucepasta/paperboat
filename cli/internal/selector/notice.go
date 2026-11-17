package selector

import (
	"sync/atomic"
	"time"
)

// A notice is a one-line confirmation ("Paused tunnel x") shown in the footer of the
// next menu instead of a full-screen result viewer; it fades after noticeLifetime or
// at the next key press.
const noticeLifetime = 4 * time.Second

// A notice nobody opened a menu for within noticeStale is dropped, not shown out of context.
const noticeStale = 30 * time.Second

type queuedNotice struct {
	text string
	at   time.Time
}

var pendingNotice atomic.Pointer[queuedNotice]

// SetNotice queues text for the next chooser that opens.
func SetNotice(text string) {
	pendingNotice.Store(&queuedNotice{text: SanitizeText(text), at: time.Now()})
}

func takeNotice() string {
	if n := pendingNotice.Swap(nil); n != nil && time.Since(n.at) < noticeStale {
		return n.text
	}
	return ""
}

type noticeExpired struct{}
