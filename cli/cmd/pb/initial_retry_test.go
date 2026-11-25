package main

import (
	"testing"
	"time"
)

func TestInitialRetryIsQuickOnceThenSpaced(t *testing.T) {
	for _, c := range []struct {
		retry, seconds int
		want           time.Duration
	}{{1, 2, 200 * time.Millisecond}, {2, 2, 2 * time.Second}, {6, 2, 2 * time.Second}, {1, 0, 0}} {
		if got := initialRetryDelay(c.retry, c.seconds); got != c.want {
			t.Fatalf("retry %d of %d s waits %v, want %v", c.retry, c.seconds, got, c.want)
		}
	}
}
