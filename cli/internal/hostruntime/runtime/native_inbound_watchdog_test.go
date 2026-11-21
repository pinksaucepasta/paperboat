//go:build darwin || linux || windows

package runtime

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestInboundWatchdogTripsOnlyOnSustainedTimeoutsAndBacksOff(t *testing.T) {
	now := time.Unix(1_000, 0)
	w := &inboundWatchdog{now: func() time.Time { return now }}
	timeout := errors.Join(errors.New("handshake"), context.DeadlineExceeded)
	fail := func(n int, step time.Duration) (tripped int) {
		for range n {
			now = now.Add(step)
			if trip, _ := w.observe("accept_failed", timeout); trip {
				tripped++
			}
		}
		return tripped
	}
	if fail(20, time.Second) != 0 { // 20 failures inside one minute
		t.Fatal("tripped before the window elapsed")
	}
	a, _ := w.observe("accept_failed", errors.New("revoked peer"))
	b, _ := w.observe("dial_failed", timeout)
	if a || b {
		t.Fatal("non-timeout or outbound failure counted")
	}
	if fail(30, 2*time.Second) != 1 {
		t.Fatal("sustained inbound timeouts did not trip once")
	}
	w.reset()
	if fail(40, 2*time.Second) != 0 { // 80 s < doubled 2 min window
		t.Fatal("second trip ignored backoff")
	}
	if fail(30, 2*time.Second) != 1 {
		t.Fatal("no second trip after the doubled window")
	}
	w.observe("accepted", nil)
	if w.window() != inboundWedgeWindow {
		t.Fatal("a successful accept did not reset the backoff")
	}
	now = now.Add(time.Hour)
	if fail(4, time.Minute) != 0 || fail(10, time.Hour) != 0 {
		t.Fatal("tripped on failures spread apart, not a sustained wedge")
	}
}
