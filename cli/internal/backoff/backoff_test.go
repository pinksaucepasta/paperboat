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
