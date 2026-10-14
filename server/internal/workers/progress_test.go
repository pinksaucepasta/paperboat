package workers

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestProgressOnlyRealCompletionAndStallRecovery(t *testing.T) {
	now := time.Date(2026, 9, 19, 0, 0, 0, 0, time.UTC)
	p := NewProgress(time.Minute)
	p.now = func() time.Time { return now }
	p.Register("private-worker-secret")
	p.Register("worker_machines")
	ctx := p.Context(t.Context(), "worker_machines")
	check := func(state int, healthy, warming bool) {
		t.Helper()
		s := p.Snapshot()
		if len(s) != 1 || s[0].State != state || s[0].Healthy != healthy || s[0].Warming != warming {
			t.Fatalf("snapshot %+v", s)
		}
	}
	check(ProgressUnknown, false, true)
	finish := BeginIteration(ctx, time.Second)
	now = now.Add(30 * time.Second)
	check(ProgressRunning, false, true)
	if !p.Snapshot()[0].LastFinished.IsZero() {
		t.Fatal("timer fabricated progress")
	}
	now = now.Add(31 * time.Second)
	check(ProgressStale, false, false)
	finish(nil)
	check(ProgressCurrent, true, false)
	finish(errors.New("late duplicate"))
	check(ProgressCurrent, true, false)
	finish = BeginIteration(ctx, time.Second)
	IterationFailure(ctx, "worker_iteration", "worker_iteration_failed", errors.New("private error"))
	finish(nil)
	check(ProgressFailed, false, false)
	finish = BeginIteration(ctx, time.Second)
	check(ProgressFailed, false, false)
	finish(nil)
	check(ProgressCurrent, true, false)
	now = now.Add(66 * time.Second)
	check(ProgressStale, false, false)
	finish = BeginIteration(ctx, time.Second)
	finish(nil)
	check(ProgressCurrent, true, false)
	canceled, cancel := context.WithCancel(ctx)
	finish = BeginIteration(canceled, time.Second)
	cancel()
	finish(nil)
	check(ProgressCanceled, false, false)
	p.Exit("worker_machines", false)
	check(ProgressFailed, false, false)
}

func TestIterationObserverReceivesBoundedFirstFailureAndRecoveryOutsideLock(t *testing.T) {
	p := NewProgress(time.Minute)
	p.Register("worker_machines")
	type observed struct {
		fault     IterationFault
		recovered bool
	}
	var events []observed
	ctx := p.Context(t.Context(), "worker_machines", func(_ context.Context, fault IterationFault, recovered bool) {
		// Snapshot locks Progress.mu; this also proves callbacks run after unlock.
		_ = p.Snapshot()
		events = append(events, observed{fault: fault, recovered: recovered})
	})
	finish := BeginIteration(ctx, time.Second)
	IterationFailure(ctx, "worker_iteration", "worker_iteration_failed", errors.New("secret failure"))
	IterationFailure(ctx, "worker_iteration", "another_failure", errors.New("later failure"))
	finish(nil)
	if len(events) != 1 || events[0].fault.Code != "worker_iteration_failed" || events[0].fault.Err == nil || events[0].recovered {
		t.Fatalf("first failure was not retained for the pass: %+v", events)
	}
	if got := p.Snapshot()[0].State; got != ProgressFailed {
		t.Fatalf("absorbed error did not fail progress: %d", got)
	}
	finish = BeginIteration(ctx, time.Second)
	finish(nil)
	if len(events) != 2 || !events[1].recovered || events[1].fault.Code != "worker_recovered" || events[1].fault.Err != nil {
		t.Fatalf("successful pass did not report recovery: %+v", events)
	}
	if got := p.Snapshot()[0].State; got != ProgressCurrent {
		t.Fatalf("successful pass did not restore progress: %d", got)
	}
}

func TestIterationCancellationCarriesCancellationCause(t *testing.T) {
	p := NewProgress(time.Minute)
	p.Register("worker_machines")
	var got IterationFault
	ctx, cancel := context.WithCancel(t.Context())
	workerCtx := p.Context(ctx, "worker_machines", func(_ context.Context, fault IterationFault, _ bool) { got = fault })
	finish := BeginIteration(workerCtx, time.Second)
	IterationFailure(workerCtx, "worker_iteration", "worker_iteration_failed", syscall.EIO)
	cancel()
	finish(nil)
	if got.Code != "worker_iteration_failed" || !errors.Is(got.Err, syscall.EIO) || !errors.Is(got.Err, context.Canceled) {
		t.Fatalf("cancellation discarded the substantive iteration cause: %+v", got)
	}
	if gotState := p.Snapshot()[0].State; gotState != ProgressFailed {
		t.Fatalf("mixed failure and canceled pass state = %d, want failed", gotState)
	}
}

func TestPureIterationCancellationRemainsInformational(t *testing.T) {
	p := NewProgress(time.Minute)
	p.Register("worker_machines")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var got IterationFault
	workerCtx := p.Context(ctx, "worker_machines", func(_ context.Context, fault IterationFault, _ bool) { got = fault })
	finish := BeginIteration(workerCtx, time.Second)
	IterationFailure(workerCtx, "worker_iteration", "worker_iteration_failed", fmt.Errorf("worker stopped: %w", context.Canceled))
	cancel()
	finish(nil)
	if got.Code != "worker_canceled" || !errors.Is(got.Err, context.Canceled) || errors.Is(got.Err, syscall.EIO) {
		t.Fatalf("pure cancellation was not kept informational: %+v", got)
	}
	if gotState := p.Snapshot()[0].State; gotState != ProgressCanceled {
		t.Fatalf("pure canceled pass state = %d, want canceled", gotState)
	}
	if contextCancellationOnly(errors.Join(context.Canceled, syscall.EIO)) || contextCancellationOnly(errors.Join(context.Canceled, context.DeadlineExceeded)) {
		t.Fatal("mixed cause tree was proven to be pure cancellation")
	}
}

func TestIterationDeadlineRemainsFailureInsteadOfCancellation(t *testing.T) {
	p := NewProgress(time.Minute)
	p.Register("worker_machines")
	ctx, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer cancel()
	var got IterationFault
	workerCtx := p.Context(ctx, "worker_machines", func(_ context.Context, fault IterationFault, _ bool) { got = fault })
	finish := BeginIteration(workerCtx, time.Second)
	IterationFailure(workerCtx, "worker_iteration", "worker_iteration_failed", context.DeadlineExceeded)
	finish(nil)
	if got.Code != "worker_iteration_failed" || !errors.Is(got.Err, context.DeadlineExceeded) {
		t.Fatalf("deadline was rewritten as cancellation: %+v", got)
	}
	if gotState := p.Snapshot()[0].State; gotState != ProgressFailed {
		t.Fatalf("deadline pass state = %d, want failed", gotState)
	}
}

func TestProgressUnstartedAndConcurrentSnapshotsAreBounded(t *testing.T) {
	p := NewProgress(time.Minute)
	start := time.Now()
	p.now = func() time.Time { return start }
	for _, name := range ProgressNames() {
		p.Register(name)
	}
	for i := 0; i < 100; i++ {
		p.Register("untrusted")
	}
	if len(p.Snapshot()) != 20 {
		t.Fatal("unexpected registry cardinality")
	}
	start = start.Add(time.Minute + time.Second)
	for _, s := range p.Snapshot() {
		if s.State != ProgressStale || s.Healthy || s.Warming {
			t.Fatal(s)
		}
	}
	// Fixed clock avoids sleeps; concurrent real iteration state and reads race-test.
	var wg sync.WaitGroup
	for _, name := range ProgressNames() {
		name := name
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx := p.Context(t.Context(), name)
			for i := 0; i < 100; i++ {
				finish := BeginIteration(ctx, time.Second)
				finish(nil)
				p.Snapshot()
			}
		}()
	}
	wg.Wait()
}
