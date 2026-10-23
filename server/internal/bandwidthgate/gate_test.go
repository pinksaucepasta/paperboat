package bandwidthgate

import (
	"context"
	"testing"
	"time"
)

// A database too slow for the hot path (every lookup times out) must not keep an account over
// its allowance open for good: the background lookup settles the cached answer to denied.
func TestSlowLookupsStillSettleADenial(t *testing.T) {
	g := &Gate{timeout: 10 * time.Millisecond}
	g.lookup = func(ctx context.Context, _ string, _ time.Time) (bool, error) {
		select {
		case <-time.After(50 * time.Millisecond):
			return true, nil
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}
	if g.Denied(context.Background(), "usr_slow", time.Now()) {
		t.Fatal("hot path waited past its deadline")
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if denied, known := g.Cached("usr_slow"); known && denied {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("a slow but successful lookup never denied the account")
}
