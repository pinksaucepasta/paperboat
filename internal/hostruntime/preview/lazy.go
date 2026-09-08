package preview

import (
	"context"
	"sync"
	"time"
)

const (
	LazyIdleTimeout          = 5 * time.Minute
	LazyAbsoluteLifetime     = 8 * time.Hour
	LazyMaxStreamLifetime    = time.Hour
	LazyOriginConnectTimeout = 3 * time.Second
)

// LazyBinding fences a policy activation to the exact installed daemon boot.
// The policy authorizes the port; application process identity is deliberately
// outside this contract.
type LazyBinding struct {
	PolicyID               string `json:"policy_id"`
	PolicyGeneration       int64  `json:"policy_generation"`
	InstallationGeneration int64  `json:"installation_generation"`
	BootID                 string `json:"boot_id"`
}

func (b LazyBinding) valid() bool {
	return validLeaseID(b.PolicyID) && b.PolicyGeneration > 0 && b.InstallationGeneration > 0 && validLazyBootID(b.BootID)
}

func validLazyBootID(value string) bool {
	if len(value) < 16 || len(value) > 64 {
		return false
	}
	for _, r := range value {
		if !(r == '_' || r == '-' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

// LazyLifecycle stops a lazy lease after five minutes with no active streams.
// Each stream is independently capped at one hour and by the lease deadline.
type LazyLifecycle struct {
	done     chan struct{}
	wake     chan struct{}
	stopOnce sync.Once
	mu       sync.Mutex
	active   int
	lastIdle time.Time
	now      func() time.Time
	idle     time.Duration
	stream   time.Duration
	deadline time.Time
}

func newLazyLifecycle(ctx context.Context, now func() time.Time, idle, stream time.Duration, deadline time.Time) *LazyLifecycle {
	l := &LazyLifecycle{done: make(chan struct{}), wake: make(chan struct{}, 1), now: now, idle: idle, stream: stream, deadline: deadline.UTC(), lastIdle: now().UTC()}
	go l.run(ctx)
	return l
}

func (l *LazyLifecycle) Done() <-chan struct{} { return l.done }

func (l *LazyLifecycle) Stop() {
	if l == nil {
		return
	}
	l.stopOnce.Do(func() { close(l.done) })
	l.signal()
}

func (l *LazyLifecycle) BeginStream(parent context.Context, authorizationDeadline time.Time) (context.Context, func()) {
	l.mu.Lock()
	l.active++
	l.mu.Unlock()
	l.signal()
	deadline := l.now().UTC().Add(l.stream)
	for _, candidate := range []time.Time{l.deadline, authorizationDeadline.UTC()} {
		if !candidate.IsZero() && candidate.Before(deadline) {
			deadline = candidate
		}
	}
	ctx, cancel := context.WithDeadline(parent, deadline)
	go func() {
		select {
		case <-l.done:
			cancel()
		case <-ctx.Done():
		}
	}()
	var once sync.Once
	return ctx, func() {
		once.Do(func() {
			cancel()
			l.mu.Lock()
			l.active--
			if l.active == 0 {
				l.lastIdle = l.now().UTC()
			}
			l.mu.Unlock()
			l.signal()
		})
	}
}

func (l *LazyLifecycle) run(ctx context.Context) {
	timer := time.NewTimer(l.idle)
	defer timer.Stop()
	for {
		l.mu.Lock()
		active, lastIdle := l.active, l.lastIdle
		l.mu.Unlock()
		now := l.now().UTC()
		wait := l.deadline.Sub(now)
		if active == 0 {
			if idleWait := lastIdle.Add(l.idle).Sub(now); idleWait < wait {
				wait = idleWait
			}
		}
		if wait <= 0 {
			l.Stop()
			return
		}
		timer.Reset(wait)
		select {
		case <-ctx.Done():
			l.Stop()
			return
		case <-l.done:
			return
		case <-l.wake:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		case <-timer.C:
		}
	}
}

func (l *LazyLifecycle) signal() {
	select {
	case l.wake <- struct{}{}:
	default:
	}
}
