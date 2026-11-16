package machineevents

import (
	"testing"
	"time"
)

// A rollout ends every daemon's held wait at once; first retries must spread over the whole window, not land in the
// same second, and later retries keep backoff plus up to half again.
func TestRetryDelaySpreadsFirstRetry(t *testing.T) {
	lo, hi := time.Duration(1<<62), time.Duration(0)
	for range 2000 {
		d := retryDelay(minBackoff, true)
		if d < minBackoff || d >= minBackoff+reconnectSpread {
			t.Fatalf("first retry %v outside [%v, %v)", d, minBackoff, minBackoff+reconnectSpread)
		}
		lo, hi = min(lo, d), max(hi, d)
	}
	if hi-lo < reconnectSpread*4/5 {
		t.Fatalf("first retries span only %v of %v", hi-lo, reconnectSpread)
	}
	for _, b := range []time.Duration{2 * time.Second, maxBackoff} {
		if d := retryDelay(b, false); d < b || d > b+b/2 {
			t.Fatalf("retry after backoff %v = %v, want [%v, %v]", b, d, b, b+b/2)
		}
	}
}
