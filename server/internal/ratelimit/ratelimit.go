// Package ratelimit is the in-memory, per-client token bucket in front of the sign-in and
// credential endpoints (PLAN F21-14, Lane X #15). It runs before any database work, so one client
// cannot use up shared capacity or hold database locks by retrying credentials; database-backed
// limits (device authorization) and the operator per-email lockout stay as the second line.
package ratelimit

import (
	"sync"
	"time"
)

// Limiter allows perMinute requests per key on average, with bursts up to burst. It keeps at most
// maxKeys keys: when full, idle (refilled) keys are dropped first, then arbitrary ones, so memory
// is bounded even when a client rotates addresses.
type Limiter struct {
	mu        sync.Mutex
	perMinute float64
	burst     float64
	maxKeys   int
	now       func() time.Time
	buckets   map[string]*bucket
}

type bucket struct {
	tokens float64
	at     time.Time
}

// New returns nil, a limiter that allows everything, when perMinute or burst is zero: only a
// configuration that skipped validation (config.RateLimits requires positive values) has that.
func New(perMinute, burst, maxKeys int) *Limiter {
	if perMinute < 1 || burst < 1 {
		return nil
	}
	return &Limiter{perMinute: float64(perMinute), burst: float64(burst), maxKeys: maxKeys, now: time.Now, buckets: map[string]*bucket{}}
}

// Allow takes one token for key. When none is left it returns false and how long until one is.
func (l *Limiter) Allow(key string) (bool, time.Duration) {
	return l.take(key, true)
}

// Check reports whether key has a token, without taking it.
func (l *Limiter) Check(key string) (bool, time.Duration) {
	return l.take(key, false)
}

func (l *Limiter) take(key string, consume bool) (bool, time.Duration) {
	if l == nil {
		return true, 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	b, ok := l.buckets[key]
	if !ok {
		l.makeRoom(now)
		b = &bucket{tokens: l.burst, at: now}
		l.buckets[key] = b
	}
	b.tokens = min(l.burst, b.tokens+now.Sub(b.at).Minutes()*l.perMinute)
	b.at = now
	if b.tokens < 1 {
		return false, time.Duration((1 - b.tokens) / l.perMinute * float64(time.Minute))
	}
	if consume {
		b.tokens--
	}
	return true, 0
}

func (l *Limiter) makeRoom(now time.Time) {
	if len(l.buckets) < l.maxKeys {
		return
	}
	for key, b := range l.buckets {
		if b.tokens+now.Sub(b.at).Minutes()*l.perMinute >= l.burst {
			delete(l.buckets, key)
		}
	}
	for key := range l.buckets {
		if len(l.buckets) < l.maxKeys {
			return
		}
		delete(l.buckets, key)
	}
}
