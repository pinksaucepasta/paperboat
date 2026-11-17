package reporting

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// Fence is the shutdown fence every error reporter shares: bounded SDK enqueue
// work is admitted until the reporter closes, and Close drains admitted work
// before the SDK flushes, all inside one shutdown budget. A submission holds the
// fence only while it enqueues, never for an operation's lifetime.
//
// Prior art: none in ~/ref; this is the sentry-go Flush/Close contract plus a gate
// so a late submit cannot race Close.
type Fence struct {
	closed, sealed atomic.Bool
	mu             sync.RWMutex
	once           sync.Once
	done           chan struct{}
	status         atomic.Uint32
	dropped        atomic.Uint64
}

// Begin admits one bounded enqueue, which the caller ends with End. Sampler
// submissions (periodic metric snapshots) are still admitted after Close started,
// until the fence is sealed, so a stop hook can finish its last snapshot.
func (f *Fence) Begin(sampler bool) bool {
	if !f.mu.TryRLock() {
		f.dropped.Add(1)
		return false
	}
	if f.sealed.Load() || !sampler && f.closed.Load() {
		f.mu.RUnlock()
		f.dropped.Add(1)
		return false
	}
	return true
}

// End releases an admission from Begin.
func (f *Fence) End() { f.mu.RUnlock() }

// Closed reports whether Close has started.
func (f *Fence) Closed() bool { return f.closed.Load() }

// Done is closed when the shutdown goroutine finished; it is nil before Close.
func (f *Fence) Done() <-chan struct{} { return f.done }

// Dropped counts submissions refused by the fence.
func (f *Fence) Dropped() uint64 { return f.dropped.Load() }

// Status is the local lifecycle: drained means queued attempts finished, not that
// any HTTP response succeeded or Sentry ingested the signals.
func (f *Fence) Status() string {
	switch f.status.Load() {
	case 1:
		return "flushing"
	case 2:
		return "drained"
	case 3:
		return "timed_out"
	default:
		return "not_started"
	}
}

// Close shuts the fence once within limit. preClose, when it is not nil, runs
// first (the CLI stops its metric sampler there) and reports whether it finished
// in time; then the fence is sealed, admitted submissions finish, and drain
// flushes and closes the SDK client, returning whether the flush completed. Later
// calls wait for the same shutdown.
func (f *Fence) Close(ctx context.Context, limit time.Duration, preClose func(context.Context) bool, drain func(context.Context) bool) {
	if ctx == nil {
		ctx = context.Background()
	}
	waitCtx, stop := context.WithTimeout(ctx, limit)
	defer stop()
	f.once.Do(func() {
		f.done = make(chan struct{})
		f.closed.Store(true)
		f.status.Store(1)
		flushCtx, cancel := context.WithTimeout(ctx, limit)
		go func() {
			defer cancel()
			defer close(f.done)
			if preClose != nil && !preClose(flushCtx) {
				f.status.Store(3)
			}
			f.sealed.Store(true)
			f.mu.Lock()
			defer f.mu.Unlock()
			flushed := drain(flushCtx)
			if flushed && flushCtx.Err() == nil {
				f.status.CompareAndSwap(1, 2)
			} else {
				f.status.Store(3)
			}
		}()
	})
	select {
	case <-f.done:
	case <-waitCtx.Done():
		// Seal before returning: a late sampler must not win the race with the
		// goroutine observing the same expired deadline.
		f.sealed.Store(true)
		f.status.Store(3)
	}
}
