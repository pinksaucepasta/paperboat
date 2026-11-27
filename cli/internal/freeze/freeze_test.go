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

// Windows' clock advances in ~15 ms steps, so a stall that just ended can carry the same reading as the
// window's start (CI: Retry called once, health worker ended); that is still a stall inside the window.
func TestStalledCountsAStallEndingAtTheWindowStart(t *testing.T) {
	SimulateStall(30 * time.Second)
	mu.Lock()
	since := stallEnd
	mu.Unlock()
	if !Stalled(since, time.Second) {
		t.Fatal("a stall ending at the window's first clock reading was not counted")
	}
}

// A child of a freeze context must see what a child of context.WithTimeout sees: the workload bridge's output wait
// runs on a derived context and is polled again only on DeadlineExceeded. Reporting Canceled dropped every idle
// web terminal at its 25 s request deadline.
func TestChildrenOfAFreezeContextSeeItsDeadlineAndCancel(t *testing.T) {
	wait := func(ctx context.Context) {
		t.Helper()
		select {
		case <-ctx.Done():
		case <-time.After(2 * time.Second):
			t.Fatal("child never ended")
		}
	}
	ctx, cancel := WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	child, childCancel := context.WithCancel(ctx)
	defer childCancel()
	grandchild, gcCancel := context.WithTimeout(child, time.Hour)
	defer gcCancel()
	wait(grandchild)
	for name, c := range map[string]context.Context{"parent": ctx, "child": child, "grandchild": grandchild} {
		if c.Err() != context.DeadlineExceeded || context.Cause(c) != context.DeadlineExceeded {
			t.Errorf("expired %s: err=%v cause=%v, want DeadlineExceeded", name, c.Err(), context.Cause(c))
		}
	}

	ctx2, cancel2 := WithTimeout(context.Background(), time.Hour)
	child2, childCancel2 := context.WithCancel(ctx2)
	defer childCancel2()
	cancel2()
	if ctx2.Err() != context.Canceled {
		t.Fatalf("cancel is not synchronous: err=%v", ctx2.Err())
	}
	wait(child2)
	if child2.Err() != context.Canceled || context.Cause(child2) != context.Canceled {
		t.Errorf("canceled child: err=%v cause=%v", child2.Err(), context.Cause(child2))
	}

	parent, parentCancel := context.WithCancelCause(context.Background())
	why := errors.New("why")
	ctx3, cancel3 := WithTimeout(parent, time.Hour)
	defer cancel3()
	parentCancel(why)
	wait(ctx3)
	if ctx3.Err() != context.Canceled || context.Cause(ctx3) != why {
		t.Errorf("parent canceled: err=%v cause=%v", ctx3.Err(), context.Cause(ctx3))
	}
}
