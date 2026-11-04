package bandwidthgate

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
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

// F21-43: 10,000 accounts do not wipe the cache (second pass is all hits), and a denied account
// stays denied through the churn when billing then turns slow.
func TestCacheKeepsTenThousandAccountsAndADeniedOne(t *testing.T) {
	var lookups atomic.Int64
	var slow atomic.Bool
	g := &Gate{timeout: 10 * time.Millisecond}
	g.lookup = func(_ context.Context, account string, _ time.Time) (bool, error) {
		lookups.Add(1)
		if slow.Load() {
			return false, errors.New("billing timed out")
		}
		return account == "usr_denied", nil
	}
	now := time.Now()
	if !g.Denied(context.Background(), "usr_denied", now) {
		t.Fatal("account over its allowance was not denied")
	}
	for pass := 0; pass < 2; pass++ {
		for i := 0; i < 10000; i++ {
			g.Denied(context.Background(), fmt.Sprintf("usr_%05d", i), now)
		}
	}
	if got := lookups.Load(); got != 10001 {
		t.Fatalf("%d lookups for 10,000 accounts read twice, want 10001: the cache lost entries", got)
	}
	slow.Store(true)
	g.mu.Lock()
	e := g.cache["usr_denied"]
	e.until = time.Now().Add(-time.Second)
	g.cache["usr_denied"] = e
	g.mu.Unlock()
	if !g.Denied(context.Background(), "usr_denied", now) {
		t.Fatal("a slow billing lookup let a denied account through")
	}
}

func TestFullCacheEvictsOnlyOneEntry(t *testing.T) {
	g := &Gate{}
	now := time.Now()
	for i := 0; i < cacheMax; i++ {
		g.put(fmt.Sprint(i), entry{until: now.Add(time.Hour)}, now)
	}
	g.put("denied", entry{denied: true, until: now.Add(time.Hour), deniedAt: now}, now)
	if len(g.cache) != cacheMax || !g.cache["denied"].denied {
		t.Fatalf("cache holds %d entries, denied kept: %v", len(g.cache), g.cache["denied"].denied)
	}
}
