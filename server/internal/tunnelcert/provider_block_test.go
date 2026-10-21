package tunnelcert

import (
	"testing"
	"time"
)

func TestProviderBlockNeverExceedsTheProviderRateLimitWindow(t *testing.T) {
	now := time.Date(2026, 10, 21, 6, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		retryAfter string
		want       time.Duration
	}{
		{"", 0},
		{"garbage", 0},
		{"10", 10 * time.Second},
		{"300", 5 * time.Minute},
		{"5640", 5 * time.Minute},
		{"86400", 5 * time.Minute},
		{now.Add(94 * time.Minute).Format("Mon, 02 Jan 2006 15:04:05 GMT"), 5 * time.Minute},
		{now.Add(30 * time.Second).Format("Mon, 02 Jan 2006 15:04:05 GMT"), 30 * time.Second},
	} {
		if got := providerBlockDelay(c.retryAfter, now); got != c.want {
			t.Fatalf("Retry-After %q blocked for %v, want %v", c.retryAfter, got, c.want)
		}
	}
}
