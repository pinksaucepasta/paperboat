package workers

import (
	"context"
	"sync/atomic"
	"testing"
)

type fakeLeadership struct {
	grants    chan chan struct{}
	released  chan int32 // the number of workers still running when leadership was released
	acquiring chan struct{}
	running   *atomic.Int32
}

func (f *fakeLeadership) Acquire(ctx context.Context) (<-chan struct{}, func(), error) {
	select {
	case f.acquiring <- struct{}{}:
	default:
	}
	select {
	case lost := <-f.grants:
		return lost, func() { f.released <- f.running.Load() }, nil
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	}
}

// A lost term stops and joins every worker before leadership is released, and a new term runs them again.
func TestLeaderSupervisorStopsWorkersOnLossAndRunsAgainOnReacquire(t *testing.T) {
	var running, starts atomic.Int32
	leader := &fakeLeadership{grants: make(chan chan struct{}), released: make(chan int32, 2), acquiring: make(chan struct{}, 1), running: &running}
	started := make(chan struct{}, 2)
	stopping := make(chan struct{}, 2)
	finish := make(chan struct{})
	s := NewLeaderSupervisor(leader, func(ctx context.Context) error {
		running.Add(1)
		starts.Add(1)
		started <- struct{}{}
		<-ctx.Done()
		stopping <- struct{}{}
		<-finish // slow shutdown: release must still wait for it
		running.Add(-1)
		return nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	<-leader.acquiring // the supervisor waits for a grant; no worker can have started without one
	if starts.Load() != 0 {
		t.Fatal("workers ran before leadership was granted")
	}
	first := make(chan struct{})
	leader.grants <- first
	<-started
	close(first)
	<-stopping // the worker was told to stop and is still shutting down
	close(finish)
	if still := <-leader.released; still != 0 {
		t.Fatalf("leadership released while %d worker(s) still ran", still)
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
