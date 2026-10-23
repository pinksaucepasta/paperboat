package workers

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

type fakeLeadership struct {
	grants   chan chan struct{}
	released chan struct{}
}

func (f *fakeLeadership) Acquire(ctx context.Context) (<-chan struct{}, func(), error) {
	select {
	case lost := <-f.grants:
		return lost, func() { f.released <- struct{}{} }, nil
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	}
}

// A lost term stops and joins every worker before leadership is released, and a new term runs them again.
func TestLeaderSupervisorStopsWorkersOnLossAndRunsAgainOnReacquire(t *testing.T) {
	leader := &fakeLeadership{grants: make(chan chan struct{}), released: make(chan struct{}, 2)}
	var running, starts atomic.Int32
	started := make(chan struct{}, 2)
	s := NewLeaderSupervisor(leader, func(ctx context.Context) error {
		running.Add(1)
		starts.Add(1)
		started <- struct{}{}
		<-ctx.Done()
		time.Sleep(20 * time.Millisecond) // slow shutdown: release must still wait for it
		running.Add(-1)
		return nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	time.Sleep(20 * time.Millisecond)
	if starts.Load() != 0 {
		t.Fatal("workers ran before leadership was granted")
	}
	first := make(chan struct{})
	leader.grants <- first
	<-started
	close(first)
	<-leader.released
	if running.Load() != 0 {
		t.Fatal("leadership released while a worker still ran")
	}
	second := make(chan struct{})
	leader.grants <- second
	<-started
	cancel()
	if err := <-done; err != context.Canceled {
		t.Fatalf("run=%v", err)
	}
	<-leader.released
	if running.Load() != 0 || starts.Load() != 2 {
		t.Fatalf("running=%d starts=%d", running.Load(), starts.Load())
	}
	if err := s.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
}
