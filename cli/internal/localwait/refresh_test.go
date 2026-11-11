package localwait

import (
	"context"
	"errors"
	"log/slog"
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

type recordCapture struct{ records chan string }

func (recordCapture) Enabled(context.Context, slog.Level) bool { return true }
func (h recordCapture) Handle(_ context.Context, r slog.Record) error {
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == "error" {
			h.records <- a.Value.String()
		}
		return true
	})
	return nil
}
func (h recordCapture) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h recordCapture) WithGroup(string) slog.Handler      { return h }

// A refresh that keeps failing is not silent (audit C #38): each distinct failure is recorded once, not every try.
func TestKeepInventoryFreshRecordsEachDistinctFailureOnce(t *testing.T) {
	handler := recordCapture{records: make(chan string, 8)}
	previous := slog.Default()
	slog.SetDefault(slog.New(handler))
	t.Cleanup(func() { slog.SetDefault(previous) })
	var calls atomic.Int32
	refresh := func(context.Context) error {
		switch n := calls.Add(1); {
		case n <= 3:
			return errors.New("control plane unreachable")
		case n <= 5:
			return errors.New("not signed in")
		}
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	KeepInventoryFresh(ctx, refresh, 5*time.Millisecond, 10*time.Millisecond)
	time.Sleep(100 * time.Millisecond) // the diagnostic queue is asynchronous
	var got []string
	for len(handler.records) > 0 {
		got = append(got, <-handler.records)
	}
	if len(got) != 2 || got[0] != "control plane unreachable" || got[1] != "not signed in" {
		t.Fatalf("records %q, want each distinct failure once", got)
	}
}
