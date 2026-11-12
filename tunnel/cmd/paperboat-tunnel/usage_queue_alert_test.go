package main

import (
	"testing"

	"github.com/pinksaucepasta/paperboat/tunnel/internal/usage"
)

func TestUsageQueueDeepAlertsAtHalf(t *testing.T) {
	for _, c := range []struct {
		reports, max int
		want         bool
	}{{0, 4096, false}, {2047, 4096, false}, {2048, 4096, true}, {4096, 4096, true}, {1, 0, false}} {
		if got := usageQueueDeep(usage.QueueStats{Reports: c.reports, MaxReports: c.max}); got != c.want {
			t.Fatalf("%d/%d = %v", c.reports, c.max, got)
		}
	}
}
