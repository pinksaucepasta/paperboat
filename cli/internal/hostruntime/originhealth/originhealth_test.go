package originhealth

import (
	"context"
	"errors"
	"net"
	"syscall"
	"testing"
	"time"
)

func TestOriginGoesDownOnlyAfterConsecutiveFailuresAndReportsTransitions(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	var events []Event
	tracker := New(func(e Event) { events = append(events, e) }, 30*time.Second, func() time.Time { return now })
	refused := &net.OpError{Err: syscall.ECONNREFUSED}

	tracker.Observe("route_1", nil)
	if len(events) != 1 || events[0].State != Up {
		t.Fatalf("first success events=%+v", events)
	}
	tracker.Observe("route_1", nil)
	for i := 0; i < FailuresToDown-1; i++ {
		tracker.Observe("route_1", refused)
	}
	if state, _ := tracker.State("route_1"); state != Up || len(events) != 1 {
		t.Fatalf("down before %d consecutive failures: state=%v events=%+v", FailuresToDown, state, events)
	}
	tracker.Observe("route_1", nil) // a success resets the streak
	for i := 0; i < FailuresToDown-1; i++ {
		tracker.Observe("route_1", refused)
	}
	if state, _ := tracker.State("route_1"); state != Up {
		t.Fatal("a success did not reset the failure streak")
	}
	tracker.Observe("route_1", refused)
	if state, reason := tracker.State("route_1"); state != Down || reason != ReasonRefused || events[len(events)-1].State != Down || events[len(events)-1].Reason != ReasonRefused {
		t.Fatalf("not down after %d consecutive failures: state=%v reason=%q events=%+v", FailuresToDown, state, reason, events)
	}
	before := len(events)
	tracker.Observe("route_1", refused)
	if len(events) != before {
		t.Fatal("a continuing outage was re-reported before the repeat interval")
	}
	now = now.Add(31 * time.Second)
	tracker.Observe("route_1", refused)
	if last := events[len(events)-1]; len(events) != before+1 || !last.Repeat || last.State != Down {
		t.Fatalf("the outage was not repeated after the interval: %+v", events[before:])
	}
	tracker.Observe("route_1", nil)
	if last := events[len(events)-1]; last.State != Up || last.Repeat {
		t.Fatalf("recovery not reported: %+v", last)
	}
}

func TestEachOriginIsTrackedSeparately(t *testing.T) {
	tracker := New(nil, 0, nil)
	for i := 0; i < FailuresToDown; i++ {
		tracker.Observe("a", syscall.ECONNREFUSED)
	}
	tracker.Observe("b", nil)
	if a, _ := tracker.State("a"); a != Down {
		t.Fatal("a is not down")
	}
	if b, _ := tracker.State("b"); b != Up {
		t.Fatal("b was affected by a")
	}
	tracker.Forget("a")
	if a, _ := tracker.State("a"); a != Unknown {
		t.Fatal("forgotten origin kept its state")
	}
}

func TestClassifyUsesFiniteReasons(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want Reason
	}{
		{&net.OpError{Err: syscall.ECONNREFUSED}, ReasonRefused},
		{context.DeadlineExceeded, ReasonTimeout},
		{syscall.ENETUNREACH, ReasonUnreachable},
		{errors.New("secret detail"), ReasonOther},
		{nil, ""},
	} {
		if got := Classify(tc.err); got != tc.want {
			t.Fatalf("Classify(%v)=%q want %q", tc.err, got, tc.want)
		}
	}
}

func TestProbeFindsAStoppedListenerAndItsReturn(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	changes := make(chan Event, 8)
	tracker := New(func(e Event) { changes <- e }, 0, nil)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go tracker.Probe(ctx, "route", 20*time.Millisecond, func(ctx context.Context) error { return ProbeTCP(ctx, "tcp", address, time.Second) })
	expect := func(state State) {
		t.Helper()
		select {
		case e := <-changes:
			if e.State != state {
				t.Fatalf("event %+v, want state %v", e, state)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("no %v event", state)
		}
	}
	expect(Up)
	_ = listener.Close()
	expect(Down)
	again, err := net.Listen("tcp", address)
	if err != nil {
		t.Skipf("port was taken: %v", err)
	}
	defer again.Close()
	expect(Up)
}
