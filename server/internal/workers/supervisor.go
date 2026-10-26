package workers

import (
	"context"
	"errors"
	"math/rand/v2"
	"sync"
	"time"
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

// runTerm starts every worker and returns nil when all return cleanly, or the context error. A worker that
// fails is restarted after a jittered backoff instead of stopping its siblings and the server (F21-16): its
// failure is already recorded by the worker's observer. joined closes once every worker has returned.
func (s *Supervisor) runTerm(ctx context.Context) (<-chan struct{}, error) {
	joined := make(chan struct{})
	var wg sync.WaitGroup
	for _, worker := range s.workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			restartOnFailure(ctx, worker)
		}()
	}
	go func() {
		wg.Wait()
		close(joined)
	}()
	if len(s.workers) == 0 {
		<-ctx.Done()
	} else {
		select {
		case <-joined:
		case <-ctx.Done():
		}
	}
	return joined, ctx.Err()
}

// failureReset is how long a restarted worker must run before its failure count starts again.
const failureReset = time.Minute

func restartOnFailure(ctx context.Context, worker Worker) {
	failures := 0
	for {
		started := time.Now()
		err := worker(ctx)
		if err == nil || ctx.Err() != nil {
			return
		}
		if time.Since(started) >= failureReset {
			failures = 0
		}
		failures++
		timer := time.NewTimer(FailureBackoff(time.Second, failures))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// FailureMaxBackoff caps the retry delay of a failing worker.
const FailureMaxBackoff = time.Minute

// FailureBackoff doubles from min(interval, 1 s) per consecutive failure up to a minute, with up to half of
// it as random jitter so instances do not retry in step.
func FailureBackoff(interval time.Duration, failures int) time.Duration {
	backoff := min(interval, time.Second) << min(failures-1, 16)
	if backoff <= 0 || backoff > FailureMaxBackoff {
		backoff = FailureMaxBackoff
	}
	half := backoff / 2
	return half + time.Duration(rand.Int64N(int64(half)+1))
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
