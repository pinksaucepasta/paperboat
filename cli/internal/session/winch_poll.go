package session

import (
	"os"
	"time"
)

// winchPollInterval is how often a console without resize signals is asked for its size.
const winchPollInterval = 250 * time.Millisecond

// pollWinch notifies ch each time size reports a different console size than
// on the previous tick, until done closes. A notification nobody has taken yet
// is not doubled: the receiver reads the size current when it handles one, so
// the last size of a resize storm wins.
func pollWinch(ch chan<- os.Signal, size func() (w, h int, err error), tick <-chan time.Time, done <-chan struct{}) {
	lastW, lastH, _ := size()
	for {
		select {
		case <-done:
			return
		case <-tick:
		}
		w, h, err := size()
		if err != nil || (w == lastW && h == lastH) {
			continue
		}
		lastW, lastH = w, h
		select {
		case ch <- os.Interrupt: // the value is unused; only the wake-up matters
		default:
		}
	}
}
