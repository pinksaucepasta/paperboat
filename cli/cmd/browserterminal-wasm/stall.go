package main

import (
	"time"

	"github.com/pinksaucepasta/paperboat/shared/terminalprotocol"
)

// stallTick is how often the browser terminal checks the machine's silence; a tick that comes much later than this
// means the page itself was frozen or throttled, not that the machine went quiet.
const stallTick = time.Second

// stallCheck reports whether the machine has been silent for terminalprotocol.StallAfter, as pb connect does. It
// returns the silence start to keep: a late tick (the worker was paused) restarts the count at now.
func stallCheck(heard, previousTick, now time.Time) (time.Time, bool) {
	if now.Sub(previousTick) > 3*stallTick {
		return now, false
	}
	return heard, now.Sub(heard) > terminalprotocol.StallAfter
}
