package workers

import (
	"context"
	"errors"
	"testing"
	"time"
)

// F21-16: a worker that fails is restarted with backoff; its siblings keep running and Run does not return.
func TestSupervisorRestartsFailingWorkerAndKeepsSiblingsRunning(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	runs := 0
	recovered := make(chan struct{})
	siblingStopped := make(chan struct{})
	supervisor := NewSupervisor(
		func(ctx context.Context) error {
			runs++
			if runs <= 3 {
				return errors.New("database unavailable")
			}
			close(recovered)
			<-ctx.Done()
			return ctx.Err()
		},
		func(ctx context.Context) error {
			<-ctx.Done()
			close(siblingStopped)
			return ctx.Err()
		},
	)
	result := make(chan error, 1)
	go func() { result <- supervisor.Run(ctx) }()
	select {
	case <-recovered:
	case err := <-result:
		t.Fatalf("supervisor stopped after a worker failure: %v", err)
	case <-ctx.Done():
		t.Fatal("failing worker was not restarted")
	}
	select {
	case <-siblingStopped:
		t.Fatal("sibling worker was stopped by another worker's failure")
	default:
	}
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("run error = %v, want cancellation", err)
	}
	if err := supervisor.Wait(context.Background()); err != nil {
		t.Fatalf("wait for workers: %v", err)
	}
}

func TestSupervisorWithNoWorkersWaitsForContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := NewSupervisor().Run(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context canceled", err)
	}
	unstartedCtx, unstartedCancel := context.WithCancel(context.Background())
	unstartedCancel()
	if err := NewSupervisor().Wait(unstartedCtx); !errors.Is(err, context.Canceled) {
		t.Fatalf("unstarted wait error = %v, want context canceled", err)
	}
}

func TestSupervisorWaitJoinsWorkerAfterCancellation(t *testing.T) {
	release := make(chan struct{})
	workerCanceled := make(chan struct{})
	supervisor := NewSupervisor(func(ctx context.Context) error {
		<-ctx.Done()
		close(workerCanceled)
		<-release
		return nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := supervisor.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("run error = %v, want canceled", err)
	}
	<-workerCanceled
	waitCtx, waitCancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer waitCancel()
	if err := supervisor.Wait(waitCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("early wait error = %v, want deadline exceeded", err)
	}
	close(release)
	if err := supervisor.Wait(context.Background()); err != nil {
		t.Fatalf("joined wait error = %v", err)
	}
}
