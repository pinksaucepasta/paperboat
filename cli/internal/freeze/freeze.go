// Package freeze is the one rule every deadline check in the host runtime follows: a deadline measures a
// hung peer, not a frozen process. When this process itself did not run for longer than the deadline
// (SIGSTOP, VM pause, memory thrash), the check restarts its window instead of firing. Prior art: Tailscale
// treats a long wall-clock jump as the host sleeping, not a network fault (net/netmon/netmon.go,
// majorTimeJumpThreshold).
package freeze

import (
	"context"
	"sync"
	"time"
)

const tick = time.Second

var (
	startOnce sync.Once
	mu        sync.Mutex
	lastTick  time.Time
	stallEnd  time.Time
	stallLen  time.Duration
)

// observe records a gap in this process's own run time: the monitor goroutine ticks every second, so a gap
// of several ticks means the whole process was not scheduled. Callers also call it so that a timer waking at
// the same instant as the monitor still sees the gap.
func observe() {
	now := time.Now()
	mu.Lock()
	if !lastTick.IsZero() && now.Sub(lastTick) > 3*tick {
		stallEnd, stallLen = now, now.Sub(lastTick)
	}
	lastTick = now
	mu.Unlock()
}

func start() {
	startOnce.Do(func() {
		observe()
		go func() {
			for range time.Tick(tick) {
				observe()
			}
		}()
	})
}

// SimulateStall records a stall of length d that just ended, as the monitor does after a process resumes. Tests only.
func SimulateStall(d time.Duration) {
	start()
	mu.Lock()
	lastTick = time.Now().Add(-d)
	mu.Unlock()
	observe()
}

// Stalled reports whether this process did not run for longer than limit at some point since `since`.
func Stalled(since time.Time, limit time.Duration) bool {
	start()
	observe()
	mu.Lock()
	defer mu.Unlock()
	// Not After: a coarse clock (Windows ~15 ms) gives a stall that just ended the same reading as `since`.
	return !stallEnd.Before(since) && stallLen > limit
}

// Gap is the checker's own view of the same rule for a loop that polls: Stalled(now, limit) is true when the
// previous call was more than limit ago, so the checker was frozen, not the thing it watches.
type Gap struct{ last time.Time }

func (g *Gap) Stalled(now time.Time, limit time.Duration) bool {
	stalled := !g.last.IsZero() && now.Sub(g.last) > limit
	g.last = now
	return stalled
}

// timeoutContext keeps its own Done channel. Embedding the inner cancelCtx's channel (11-19, b96867cd3) let Go
// attach derived contexts straight to that cancelCtx, so a child of an expired window reported Canceled, not
// DeadlineExceeded: every idle terminal's output wait through the workload bridge then failed as an owner fault at
// its 25 s request deadline and the browser reconnected. With its own channel, children watch Done and copy Err
// and Cause, as they do for context.WithTimeout.
type timeoutContext struct {
	context.Context // inner: carries values, the parent's deadline and the cause (context.Cause finds it)
	cancelInner     context.CancelCauseFunc
	done            chan struct{}
	mu              sync.Mutex
	err             error
}

func (c *timeoutContext) Done() <-chan struct{} { return c.done }

func (c *timeoutContext) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

// finish ends the context once: the cause is recorded on the inner context before Done closes, so a child that
// wakes on Done reads the right Err and Cause.
func (c *timeoutContext) finish(err, cause error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return
	}
	c.cancelInner(cause)
	c.err = err
	close(c.done)
}

// WithTimeout is context.WithTimeout whose clock restarts when the process was frozen for longer than d
// during the window.
func WithTimeout(parent context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	start()
	inner, cancelInner := context.WithCancelCause(parent)
	ctx := &timeoutContext{Context: inner, cancelInner: cancelInner, done: make(chan struct{})}
	begin := time.Now()
	var mu sync.Mutex
	var timer *time.Timer
	timer = time.AfterFunc(d, func() {
		mu.Lock()
		defer mu.Unlock()
		if Stalled(begin, d) {
			begin = time.Now()
			timer.Reset(d)
			return
		}
		ctx.finish(context.DeadlineExceeded, context.DeadlineExceeded)
	})
	// The parent ending ends this context with the parent's error; the cause stays the parent's.
	stopParent := context.AfterFunc(inner, func() { ctx.finish(parent.Err(), context.Cause(parent)) })
	if parent.Err() != nil {
		ctx.finish(parent.Err(), context.Cause(parent))
	}
	return ctx, func() {
		mu.Lock()
		timer.Stop()
		mu.Unlock()
		stopParent()
		ctx.finish(context.Canceled, context.Canceled)
	}
}

// Retry runs call, and once more when it failed while the process was frozen for longer than limit: the
// failure is then the freeze's deadline, not the peer's.
func Retry(ctx context.Context, limit time.Duration, call func() error) error {
	begin := time.Now()
	err := call()
	if err != nil && ctx.Err() == nil && Stalled(begin, limit) {
		return call()
	}
	return err
}
