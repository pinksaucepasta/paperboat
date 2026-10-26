// Package backoff is the one retry schedule for control-plane calls: doubling waits with
// multiplicative jitter, the shape tailscaled uses (util/backoff), so machines that failed together
// (a server deploy drops every connection at once) do not retry together.
package backoff

import (
	"math/rand/v2"
	"time"
)

// Jitter spreads d uniformly over [d/2, 3d/2).
func Jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	return d/2 + time.Duration(rand.Int64N(int64(d)))
}

// Retry is the wait before retry n (1 for the first) of a failing call: base doubling to max, then
// jittered. n below 1 counts as 1.
func Retry(n int, base, max time.Duration) time.Duration {
	delay := max
	if n < 1 {
		n = 1
	}
	if n < 32 && base<<(n-1) < max {
		delay = base << (n - 1)
	}
	return Jitter(delay)
}
