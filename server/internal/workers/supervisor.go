package workers

import (
	"context"
	"errors"
	"sync"
)

type Worker func(context.Context) error

type Supervisor struct {
	workers []Worker
	leader  Leadership

	mu      sync.Mutex
	started bool
	ready   chan struct{}
	done    chan struct{}
}

func NewSupervisor(workers ...Worker) *Supervisor {
	return &Supervisor{workers: workers, ready: make(chan struct{}), done: make(chan struct{})}
}

// NewLeaderSupervisor runs the workers only while leader grants this instance the right to. When the
// right is lost, every worker is stopped and joined before release, and Run waits for it again.
func NewLeaderSupervisor(leader Leadership, workers ...Worker) *Supervisor {
	s := NewSupervisor(workers...)
	s.leader = leader
	return s
}

var ErrSupervisorAlreadyStarted = errors.New("worker supervisor already started")

func (s *Supervisor) Run(ctx context.Context) error {
	if s == nil || ctx == nil {
		return context.Canceled
	}
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return ErrSupervisorAlreadyStarted
	}
	s.started = true
	close(s.ready)
	s.mu.Unlock()
	if s.leader == nil {
		joined, err := s.runTerm(ctx)
		go func() { <-joined; close(s.done) }()
		return err
	}
	defer close(s.done)
	for {
		lost, release, err := s.leader.Acquire(ctx)
		if err != nil {
			return err
		}
		termCtx, cancel := context.WithCancel(ctx)
		go func() {
			select {
			case <-lost:
				cancel()
			case <-termCtx.Done():
			}
		}()
		joined, err := s.runTerm(termCtx)
		cancel()
		<-joined
		release()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		select {
		case <-lost:
			continue // another instance leads now; wait for the right again
		default:
		}
		return err
	}
}

// runTerm starts every worker and returns the first worker error, nil when all return cleanly, or the
// context error. joined closes once every worker has returned.
func (s *Supervisor) runTerm(ctx context.Context) (<-chan struct{}, error) {
	joined := make(chan struct{})
	if len(s.workers) == 0 {
		<-ctx.Done()
		close(joined)
		return joined, ctx.Err()
	}
	ctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	errs := make(chan error, len(s.workers))
	for _, worker := range s.workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := worker(ctx); err != nil {
				errs <- err
			}
		}()
	}
	go func() {
		wg.Wait()
		close(joined)
		cancel()
	}()
	select {
	case err := <-errs:
		cancel()
		return joined, err
	case <-joined:
		// A worker may return an error immediately before the join goroutine
		// closes joined. Prefer that queued cause when both cases are ready so a
		// fast worker failure cannot be reported as a clean shutdown.
		select {
		case err := <-errs:
			return joined, err
		default:
			return joined, nil
		}
	case <-ctx.Done():
		cancel()
		return joined, ctx.Err()
	}
}

// Wait blocks until every worker started by Run has returned. Run deliberately
// reports the first worker or parent-context failure promptly so callers can
// begin shutting down ingress; Wait is the ownership boundary that must finish
// before dependencies used by workers are closed.
func (s *Supervisor) Wait(ctx context.Context) error {
	if s == nil || ctx == nil {
		return context.Canceled
	}
	select {
	case <-s.ready:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-s.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
