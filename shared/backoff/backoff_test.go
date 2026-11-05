package backoff

import (
	"testing"
	"time"
)

func TestRetryDoublesToTheCapWithSpreadJitter(t *testing.T) {
	for n, want := range map[int]time.Duration{0: time.Second, 1: time.Second, 2: 2 * time.Second, 3: 4 * time.Second, 7: time.Minute, 40: time.Minute} {
		seen := map[time.Duration]bool{}
		for range 200 {
			got := Retry(n, time.Second, time.Minute)
			if got < want/2 || got >= want/2*3 {
				t.Fatalf("Retry(%d) = %v outside [%v, %v)", n, got, want/2, want/2*3)
			}
			seen[got] = true
		}
		if len(seen) < 50 {
			t.Fatalf("Retry(%d) barely spreads: %d distinct waits of 200", n, len(seen))
		}
	}
	if Jitter(0) != 0 {
		t.Fatal("Jitter(0) waits")
	}
}

// Long failure streaks keep waiting the cap: doubling a large base must not overflow into a zero wait.
func TestRetryNeverOverflowsIntoAZeroWait(t *testing.T) {
	for _, c := range []struct{ base, max time.Duration }{{5 * time.Minute, 6 * time.Hour}, {10 * time.Second, time.Minute}, {15 * time.Second, 5 * time.Minute}, {time.Second, time.Minute}} {
		for n := 1; n <= 200; n++ {
			if got := Retry(n, c.base, c.max); got <= 0 || got >= c.max/2*3 {
				t.Fatalf("Retry(%d, %v, %v) = %v, want within (0, %v)", n, c.base, c.max, got, c.max/2*3)
			}
		}
		if got := Retry(1000, c.base, c.max); got < c.max/2 {
			t.Fatalf("Retry(1000, %v, %v) = %v, below the capped range", c.base, c.max, got)
		}
	}
}
