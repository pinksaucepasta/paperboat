package freeze

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestGapStalledTable(t *testing.T) {
	base := time.Now()
	for _, c := range []struct {
		name  string
		gaps  []time.Duration
		limit time.Duration
		want  bool
	}{
		{"first call", []time.Duration{0}, time.Minute, false},
		{"regular ticks", []time.Duration{0, 5 * time.Second, 5 * time.Second}, time.Minute, false},
		{"exactly the limit", []time.Duration{0, time.Minute}, time.Minute, false},
		{"gap over the limit", []time.Duration{0, 5 * time.Second, time.Minute + time.Second}, time.Minute, true},
		{"window restarts after the gap", []time.Duration{0, 2 * time.Minute, 5 * time.Second}, time.Minute, false},
	} {
		var g Gap
		now, got := base, false
		for _, gap := range c.gaps {
			now = now.Add(gap)
			got = g.Stalled(now, c.limit)
		}
		if got != c.want {
			t.Errorf("%s: stalled=%v want %v", c.name, got, c.want)
		}
	}
}

func TestWithTimeoutIgnoresAFreezeButFiresOnAStuckPeer(t *testing.T) {
	// Not frozen: fires after d with DeadlineExceeded.
	ctx, cancel := WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	select {
	case <-ctx.Done():
		if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
			t.Fatalf("err=%v", ctx.Err())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a stuck peer's timeout never fired")
	}
	// Frozen during the window: the timer restarts, then fires for real.
	begin := time.Now()
	ctx2, cancel2 := WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel2()
	time.Sleep(20 * time.Millisecond)
	SimulateStall(10 * time.Second)
	select {
	case <-ctx2.Done():
		if time.Since(begin) < 150*time.Millisecond {
			t.Fatalf("fired at %s: the freeze did not restart the window", time.Since(begin))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("window restarted but never fired")
	}
}

func TestRetryRetriesOnlyAfterAFreeze(t *testing.T) {
	fail := errors.New("deadline")
	calls := 0
	err := Retry(context.Background(), time.Second, func() error { calls++; return fail })
	if !errors.Is(err, fail) || calls != 1 {
		t.Fatalf("no freeze: calls=%d err=%v", calls, err)
	}
	calls = 0
	err = Retry(context.Background(), time.Second, func() error {
		calls++
		if calls == 1 {
			SimulateStall(30 * time.Second)
			return fail
		}
		return nil
	})
	if err != nil || calls != 2 {
		t.Fatalf("freeze: calls=%d err=%v", calls, err)
	}
}
