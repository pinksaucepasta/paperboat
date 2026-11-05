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
	// Doubling stops at max, so it never overflows: base<<(n-1) wrapped negative for a large base·2^(n-1)
	// (5 min at n=26, 10 s at n=31) and the wait became 0, a retry loop with no wait.
	delay := base
	for doubled := 1; doubled < n && delay > 0 && delay < max; doubled++ {
		delay *= 2
	}
	return Jitter(min(delay, max))
}
