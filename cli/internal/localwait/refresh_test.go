package localwait

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// A waiting command asks for inventory refreshes at growing intervals up to a maximum, never overlapping,
// and stops asking when the wait ends.
func TestKeepInventoryFreshBacksOffAndStopsWithTheWait(t *testing.T) {
	var inFlight, overlapped atomic.Int32
	var mu sync.Mutex
	var at []time.Time
	refresh := func(context.Context) error {
		if inFlight.Add(1) > 1 {
			overlapped.Store(1)
		}
		mu.Lock()
		at = append(at, time.Now())
		mu.Unlock()
		inFlight.Add(-1)
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 700*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	go func() { KeepInventoryFresh(ctx, refresh, 20*time.Millisecond, 160*time.Millisecond); close(done) }()
	<-done
	mu.Lock()
	calls := append([]time.Time(nil), at...)
	mu.Unlock()
	// Waits 20, 40, 80, 160, 160, ... ms: about 6 calls in 700 ms (a fixed 20 ms pace would make ~35).
	if len(calls) < 4 || len(calls) > 8 {
		t.Fatalf("refreshes in 700 ms = %d, want about 6", len(calls))
	}
	for i := 2; i < len(calls); i++ {
		gap, previous := calls[i].Sub(calls[i-1]), calls[i-1].Sub(calls[i-2])
		if gap < previous-10*time.Millisecond && gap < 150*time.Millisecond {
			t.Fatalf("refresh gaps shrank: %v then %v", previous, gap)
		}
	}
	if overlapped.Load() != 0 {
		t.Fatal("refreshes overlapped")
	}
	time.Sleep(200 * time.Millisecond)
	mu.Lock()
	after := len(at)
	mu.Unlock()
	if after != len(calls) {
		t.Fatalf("refreshes continued after the wait ended: %d -> %d", len(calls), after)
	}
}
