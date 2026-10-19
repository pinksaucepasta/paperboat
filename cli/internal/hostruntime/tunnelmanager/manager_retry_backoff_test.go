package tunnelmanager

import (
	"context"
	"testing"
	"time"
)

// A tunnel the control plane keeps refusing (a deleted one answers carrier_unavailable) used to
// be retried on every 5-second tick, each tick a burst of requests.
func TestScheduledReconcileBacksOffAFailingTunnelButExplicitReconcileDoesNot(t *testing.T) {
	now := time.Date(2026, 10, 19, 1, 0, 0, 0, time.UTC)
	store := &memoryStateStore{state: tunnelState(t, 1, 0), revision: 1}
	factory := &fakeFactory{candidates: map[uint64][]*fakeCandidate{}, err: map[uint64]error{1: ErrConnectorUnavailable}}
	manager, err := New(Config{Store: store, Factory: factory, HostID: "host_01", Clock: func() time.Time { return now }, ReconcileInterval: 5 * time.Second, ApplyTimeout: time.Second, DrainTimeout: time.Second, Report: func(Observation) {}})
	if err != nil {
		t.Fatal(err)
	}
	manager.mu.Lock()
	manager.started = true
	manager.ctx, manager.stop = context.WithCancel(context.Background())
	manager.mu.Unlock()
	defer manager.stop()
	attempts := func() int { factory.mu.Lock(); defer factory.mu.Unlock(); return len(factory.requests) }
	pass := func(scheduled bool) { _ = manager.reconcile(context.Background(), scheduled) }

	pass(true)
	if attempts() != 1 {
		t.Fatalf("first scheduled pass attempts=%d", attempts())
	}
	pass(true)
	if attempts() != 1 {
		t.Fatalf("scheduled pass inside the backoff retried: attempts=%d", attempts())
	}
	now = now.Add(6 * time.Second)
	pass(true)
	if attempts() != 2 {
		t.Fatalf("scheduled pass after the first backoff attempts=%d", attempts())
	}
	now = now.Add(6 * time.Second) // second failure doubled the wait to 10 s
	pass(true)
	if attempts() != 2 {
		t.Fatalf("backoff did not grow: attempts=%d", attempts())
	}
	pass(false)
	if attempts() != 3 {
		t.Fatalf("explicit reconcile must not be delayed: attempts=%d", attempts())
	}
}

func TestReconcileRetryBackoffDoublesToAMinute(t *testing.T) {
	now := time.Date(2026, 10, 19, 1, 0, 0, 0, time.UTC)
	var r reconcileRetry
	want := []time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second, 40 * time.Second, time.Minute, time.Minute}
	for i, delay := range want {
		r = r.failed(7, now, 5*time.Second)
		if got := r.next.Sub(now); got != delay {
			t.Fatalf("failure %d waits %v, want %v", i+1, got, delay)
		}
	}
	if r = r.failed(8, now, 5*time.Second); r.failures != 1 {
		t.Fatalf("a new desired generation kept the old backoff: %+v", r)
	}
}
