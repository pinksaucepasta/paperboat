package localwait

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// A waiting command asks for at most one inventory refresh per interval, never overlapping, and stops asking
// when the wait ends.
func TestKeepInventoryFreshPacesRefreshesAndStopsWithTheWait(t *testing.T) {
	var calls, inFlight, overlapped atomic.Int32
	refresh := func(context.Context) error {
		if inFlight.Add(1) > 1 {
			overlapped.Store(1)
		}
		calls.Add(1)
		time.Sleep(15 * time.Millisecond) // slower than the interval: the next call waits
		inFlight.Add(-1)
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	go func() { KeepInventoryFresh(ctx, refresh, 20*time.Millisecond); close(done) }()
	<-done
	got := calls.Load()
	if got < 3 || got > 10 {
		t.Fatalf("refreshes in 200 ms at a 20 ms interval = %d, want about 7", got)
	}
	if overlapped.Load() != 0 {
		t.Fatal("refreshes overlapped")
	}
	time.Sleep(60 * time.Millisecond)
	if after := calls.Load(); after != got {
		t.Fatalf("refreshes continued after the wait ended: %d -> %d", got, after)
	}
}
