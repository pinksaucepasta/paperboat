package native

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestInboundWatchdogTripsOnlyOnSustainedTimeoutsAndBacksOff(t *testing.T) {
	now := time.Unix(1_000, 0)
	w := &InboundWatchdog{Now: func() time.Time { return now }}
	timeout := Event{Kind: "accept_failed", Err: errors.Join(errors.New("handshake"), context.DeadlineExceeded)}
	fail := func(n int, step time.Duration) (tripped int) {
		for range n {
			now = now.Add(step)
			if trip, _ := w.Observe(timeout); trip {
				tripped++
			}
		}
		return tripped
	}
	if fail(20, time.Second) != 0 { // 20 failures inside one minute
		t.Fatal("tripped before the window elapsed")
	}
	a, _ := w.Observe(Event{Kind: "accept_failed", Err: errors.New("revoked peer")})
	b, _ := w.Observe(Event{Kind: "dial_failed", Err: timeout.Err})
	if a || b {
		t.Fatal("non-timeout or outbound failure counted")
	}
	if fail(30, 2*time.Second) != 1 {
		t.Fatal("sustained inbound timeouts did not trip once")
	}
	w.Reset()
	if fail(40, 2*time.Second) != 0 { // 80 s < doubled 2 min window
		t.Fatal("second trip ignored backoff")
	}
	if fail(30, 2*time.Second) != 1 {
		t.Fatal("no second trip after the doubled window")
	}
	if !w.CommitRebuild() {
		t.Fatal("a trip with no accept since did not need its rebuild")
	}
	w.Observe(Event{Kind: "accepted"}) // lands during the committed teardown
	if w.windowLocked() == inboundWedgeWindow {
		t.Fatal("an accept during a committed rebuild reset the backoff")
	}
	w.Reset()
	if fail(125, 2*time.Second) != 1 { // third trip needs the 4 min window
		t.Fatal("no third trip")
	}
	w.Observe(Event{Kind: "accepted"})
	if w.CommitRebuild() || w.windowLocked() != inboundWedgeWindow {
		t.Fatal("a successful accept did not cancel the trip and reset the backoff")
	}
	now = now.Add(time.Hour)
	if fail(4, time.Minute) != 0 || fail(10, time.Hour) != 0 {
		t.Fatal("tripped on failures spread apart, not a sustained wedge")
	}
}
