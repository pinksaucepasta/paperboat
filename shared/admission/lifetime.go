// Package admission separates short-lived admission authority from the lifetime
// of accepted work. Expiry is checked once; explicit revocation remains live.
package admission

import (
	"context"
	"errors"
	"io"
	"math/rand/v2"
	"sync"
	"time"
)

var ErrExpired = errors.New("admission credential expired")
var ErrRevoked = errors.New("admitted access revoked")

// Lease owns one admitted connection or operation. Close it when work completes
// or disconnects. Neither the credential nor descriptor is retained.
type Lease struct {
	mu       sync.Mutex
	workers  sync.WaitGroup
	closed   bool
	cleanups []func()

	ctx    context.Context
	cancel context.CancelCauseFunc
}

// Admit checks the credential deadline before accepting work, and subscribes to
// revocation before returning. A zero deadline is for already verified admission
// boundaries whose verifier owns the expiry check.
func Admit(parent context.Context, expiresAt time.Time, revoked <-chan struct{}) (*Lease, error) {
	if err := parent.Err(); err != nil {
		return nil, err
	}
	if !expiresAt.IsZero() && !time.Now().Before(expiresAt) {
		return nil, ErrExpired
	}
	ctx, cancel := context.WithCancelCause(parent)
	lease := &Lease{ctx: ctx, cancel: cancel}
	if revoked != nil {
		select {
		case <-revoked:
			cancel(ErrRevoked)
			return nil, ErrRevoked
		default:
		}
		lease.start(func() {
			select {
			case <-revoked:
				cancel(ErrRevoked)
			case <-ctx.Done():
			}
		})
	}
	return lease, nil
}

func (l *Lease) Context() context.Context { return l.ctx }
func (l *Lease) Close() {
	l.mu.Lock()
	l.closed = true
	l.cancel(context.Canceled)
	cleanups := l.cleanups
	l.cleanups = nil
	l.mu.Unlock()
	for _, cleanup := range cleanups {
		cleanup()
	}
	l.workers.Wait()
}
func (l *Lease) Revoke()      { l.cancel(ErrRevoked) }
func (l *Lease) Check() error { return context.Cause(l.ctx) }

// Bind interrupts blocked I/O on disconnect or revocation. The returned stop
// function detaches the binding after the caller has closed its owned resource.
func (l *Lease) Bind(connection io.Closer) func() bool {
	return context.AfterFunc(l.ctx, func() { _ = connection.Close() })
}

// Watch observes authoritative state until this lease ends. Capture the change
// signal before checking state so concurrent withdrawal cannot be missed.
func (l *Lease) Watch(changes func() <-chan struct{}, allowed func() bool) error {
	changed := changes()
	if !allowed() {
		l.Revoke()
		return ErrRevoked
	}
	l.start(func() {
		for {
			select {
			case <-l.ctx.Done():
				return
			case <-changed:
			}
			changed = changes()
			if !allowed() {
				l.Revoke()
				return
			}
		}
	})
	return nil
}

// OnDone releases lease-owned resources exactly once on any lifetime exit.
// Close joins these releases; callbacks must not call Close on this lease.
func (l *Lease) OnDone(release func()) {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		release()
		return
	}
	l.workers.Add(1)
	var once sync.Once
	cleanup := func() { once.Do(func() { defer l.workers.Done(); release() }) }
	stop := context.AfterFunc(l.ctx, cleanup)
	l.cleanups = append(l.cleanups, func() { stop(); cleanup() })
	l.mu.Unlock()
}

func (l *Lease) start(work func()) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed || l.ctx.Err() != nil {
		return false
	}
	l.workers.Add(1)
	go func() { defer l.workers.Done(); work() }()
	return true
}

// RevocationInterval is the single cadence and lookup bound for authority
// polling. Notifications should be used instead wherever they are available.
const RevocationInterval = time.Second

// MaxPollBackoff bounds the wait between lookups after consecutive transient failures, and
// MaxPollRetryAfter bounds a server's Retry-After hint.
const (
	MaxPollBackoff    = 30 * time.Second
	MaxPollRetryAfter = 5 * time.Minute
)

// Poll adapts authoritative state without notifications. Only ErrRevoked ends
// admitted work; transient lookup errors retain it. Each lookup is bounded,
// cancellation interrupts it, and stop waits for the worker to exit. Check must
// honor its context and use Revoke rather than Close from inside the worker.
// After the first transient failure the next lookup still follows on the next
// tick; further consecutive failures back off with jitter (see pollFailureDelay),
// so a fleet does not retry an unavailable control plane every second.
func (l *Lease) Poll(check func(context.Context) error) func() {
	ctx, cancel := context.WithCancel(l.ctx)
	done := make(chan struct{})
	if !l.start(func() {
		defer close(done)
		ticker := time.NewTicker(RevocationInterval)
		defer ticker.Stop()
		failures := 0
		for {
			if ctx.Err() != nil {
				return
			}
			bounded, stop := context.WithTimeout(ctx, RevocationInterval)
			err := check(bounded)
			stop()
			if errors.Is(err, ErrRevoked) {
				l.Revoke()
				return
			}
			if err == nil {
				failures = 0
			} else {
				failures++
			}
			wake, timer := ticker.C, (*time.Timer)(nil)
			if delay := pollFailureDelay(failures, err); delay > 0 {
				timer = time.NewTimer(delay)
				wake = timer.C
			}
			select {
			case <-ctx.Done():
			case <-wake:
			}
			if timer != nil {
				timer.Stop()
			}
		}
	}) {
		close(done)
	}
	return func() { cancel(); <-done }
}

// pollFailureDelay is how long Poll waits after a lookup instead of the next tick: zero while
// lookups succeed and after the first failure, then RevocationInterval doubled per consecutive
// failure up to MaxPollBackoff with equal jitter. A failure that carries a Retry-After hint
// (RetryAfter() time.Duration) waits at least that long, up to MaxPollRetryAfter.
func pollFailureDelay(failures int, err error) time.Duration {
	if err == nil {
		return 0
	}
	var delay time.Duration
	if failures > 1 {
		base := min(RevocationInterval<<min(failures-1, 16), MaxPollBackoff)
		delay = base/2 + rand.N(base/2+1)
	}
	var hint interface{ RetryAfter() time.Duration }
	if errors.As(err, &hint) {
		delay = max(delay, min(hint.RetryAfter(), MaxPollRetryAfter))
	}
	return delay
}
