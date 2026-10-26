package tailnet

import (
	"sync"
	"time"
)

// renewalBudget bounds the configuration renewals of every engine in this process, whatever asked for them
// (cadence, regional recovery, on-demand): one per 12 s, bursts of 2. A machine runs two engines on one
// credential, each in its own process (host runtime and __runtime-carrier), so a machine stays within 10 a
// minute, and each engine still renews its relay observations (valid 15 s) in time. A renewal over budget is
// not dropped; it waits for the next token, so requests in between coalesce into it. A 429 holds the budget for the
// server's Retry-After. Uncoupled triggers drove one machine credential past 150 calls a minute
// (hp 2026-10-27, dev-e5cebaac and dev-b7651a0a).
var renewalBudget = &tokenBucket{refill: 12 * time.Second, burst: 2}

type tokenBucket struct {
	mu           sync.Mutex
	refill       time.Duration
	burst        float64
	tokens       float64
	last         time.Time
	blockedUntil time.Time
	// granted and deferred count renewals sent and renewals that had to wait.
	granted, deferred uint64
}

// take spends a token and returns 0, or returns how long until one is available.
func (b *tokenBucket) take(now time.Time) time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.last.IsZero() {
		b.tokens, b.last = b.burst, now
	}
	if elapsed := now.Sub(b.last); elapsed > 0 {
		b.tokens = min(b.burst, b.tokens+float64(elapsed)/float64(b.refill))
		b.last = now
	}
	if wait := b.blockedUntil.Sub(now); wait > 0 {
		b.deferred++
		return wait
	}
	if b.tokens >= 1 {
		b.tokens--
		b.granted++
		return 0
	}
	b.deferred++
	return time.Duration((1 - b.tokens) * float64(b.refill))
}

// hold blocks renewals for d (a 429's Retry-After, at least one refill).
func (b *tokenBucket) hold(now time.Time, d time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if until := now.Add(max(d, b.refill)); until.After(b.blockedUntil) {
		b.blockedUntil = until
	}
}
