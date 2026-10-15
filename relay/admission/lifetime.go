// Package admission separates short-lived admission authority from the lifetime
// of accepted work. Expiry is checked once; explicit revocation remains live.
package admission

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"
)

var ErrExpired = errors.New("admission credential expired")
var ErrRevoked = errors.New("admitted access revoked")

// Lease owns one admitted connection or operation. Close it when work completes
// or disconnects. Neither the credential nor descriptor is retained.
type Lease struct {
	mu      sync.Mutex
	workers sync.WaitGroup
	closed  bool

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
	l.mu.Unlock()
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

// Poll adapts authoritative state without notifications. Only ErrRevoked ends
// admitted work; transient lookup errors retain it. Each lookup is bounded,
// cancellation interrupts it, and stop waits for the worker to exit. Check must
// honor its context and use Revoke rather than Close from inside the worker.
func (l *Lease) Poll(check func(context.Context) error) func() {
	ctx, cancel := context.WithCancel(l.ctx)
	done := make(chan struct{})
	if !l.start(func() {
		defer close(done)
		ticker := time.NewTicker(RevocationInterval)
		defer ticker.Stop()
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
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}) {
		close(done)
	}
	return func() { cancel(); <-done }
}
