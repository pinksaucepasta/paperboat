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
	now = now.Add(7 * time.Second) // the first wait is 5 s plus at most a quarter
	pass(true)
	if attempts() != 2 {
		t.Fatalf("scheduled pass after the first backoff attempts=%d", attempts())
	}
	now = now.Add(6 * time.Second) // second failure doubled the wait to 10 s or more
	pass(true)
	if attempts() != 2 {
		t.Fatalf("backoff did not grow: attempts=%d", attempts())
	}
	pass(false)
	if attempts() != 3 {
		t.Fatalf("explicit reconcile must not be delayed: attempts=%d", attempts())
	}
}

func TestReconcileRetryBackoffDoublesToAMinuteWithASpread(t *testing.T) {
	now := time.Date(2026, 10, 19, 1, 0, 0, 0, time.UTC)
	var r reconcileRetry
	base := []time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second, 40 * time.Second, 48 * time.Second, 48 * time.Second}
	for i, want := range base {
		r = r.failed(7, now, 5*time.Second, "tunnel_a")
		if got := r.next.Sub(now); got < want || got > want+want/4 || got > time.Minute {
			t.Fatalf("failure %d waits %v, want %v plus at most a quarter", i+1, got, want)
		}
	}
	other := reconcileRetry{}
	for range base {
		other = other.failed(7, now, 5*time.Second, "tunnel_b")
	}
	if r.next == other.next {
		t.Fatal("two tunnels share one retry time at the cap")
	}
	if r = r.failed(8, now, 5*time.Second, "tunnel_a"); r.failures != 1 {
		t.Fatalf("a new desired generation kept the old backoff: %+v", r)
	}
}
