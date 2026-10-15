// Package reattach owns bounded recovery after an admitted connection is lost.
// Credential expiry is never a reason to interrupt a healthy connection.
package reattach

import (
	"context"
	"errors"
	"sync"
	"time"
)

var ErrExhausted = errors.New("connection recovery attempt budget exhausted")

type Policy struct {
	// Window starts at the first transport failure, never at admission or TTL.
	Window    time.Duration
	Attempts  int
	Backoff   func(int) time.Duration
	Retryable func(error) bool
}

// Recovery is owned by one command or connection manager. Successful recovery
// does not replenish its budget; repeated disconnects cannot restart it.
type Recovery struct {
	mu              sync.Mutex
	deadline        time.Time
	policy          Policy
	remaining, used int
}

func New(policy Policy) *Recovery  { return &Recovery{policy: policy, remaining: policy.Attempts} }
func (r *Recovery) Remaining() int { return r.remaining }

// Run obtains fresh admission for every attempt. It never replays application
// bytes: the caller supplies the same operation identity and replay cursor.
func (r *Recovery) Run(ctx context.Context, attempt func(context.Context) error) error {
	if r.remaining <= 0 {
		return ErrExhausted
	}
	for r.remaining > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		r.remaining--
		r.used++
		if r.policy.Backoff != nil {
			if err := Wait(ctx, r.policy.Backoff(r.used)); err != nil {
				return err
			}
		}
		err := attempt(ctx)
		if err == nil {
			return nil
		}
		if r.policy.Retryable == nil || !r.policy.Retryable(err) || r.remaining == 0 {
			return err
		}
	}
	return ErrExhausted
}

// AfterFailure starts one fixed recovery window shared by all lost-connection
// attempts in an operation. Repeated failures cannot restart the window.
func (r *Recovery) AfterFailure(ctx context.Context) (context.Context, context.CancelFunc) {
	r.mu.Lock()
	if r.deadline.IsZero() && r.policy.Window > 0 {
		r.deadline = time.Now().Add(r.policy.Window)
	}
	r.mu.Unlock()
	return r.RequestContext(ctx)
}

// RequestContext applies an already-started recovery budget. Before transport
// loss it preserves the caller's lifetime without creating a deadline.
func (r *Recovery) RequestContext(ctx context.Context) (context.Context, context.CancelFunc) {
	r.mu.Lock()
	deadline := r.deadline
	r.mu.Unlock()
	if deadline.IsZero() {
		return ctx, func() {}
	}
	return context.WithDeadline(ctx, deadline)
}

func Wait(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
