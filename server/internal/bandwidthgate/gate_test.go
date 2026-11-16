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

// A full cache of recent denials keeps every one of them: a non-denied answer goes uncached, and a new denial
// replaces only the one confirmed longest ago.
func TestFullCacheOfDenialsEvictsNoneForAnAllowedAccount(t *testing.T) {
	g := &Gate{}
	now := time.Now()
	for i := 0; i < cacheMax; i++ {
		g.put(fmt.Sprint(i), entry{denied: true, until: now.Add(time.Hour), deniedAt: now.Add(time.Duration(i) * time.Millisecond)}, now)
	}
	g.put("allowed", entry{until: now.Add(time.Hour)}, now)
	if _, ok := g.cache["allowed"]; ok || len(g.cache) != cacheMax {
		t.Fatalf("an allowed account displaced a denial: cached=%v size=%d", ok, len(g.cache))
	}
	for i := 0; i < cacheMax; i++ {
		if !g.cache[fmt.Sprint(i)].denied {
			t.Fatalf("denial %d was evicted", i)
		}
	}
	g.put("newly_denied", entry{denied: true, until: now.Add(time.Hour), deniedAt: now.Add(time.Hour)}, now)
	if _, ok := g.cache["0"]; ok || !g.cache["newly_denied"].denied || len(g.cache) != cacheMax {
		t.Fatal("a new denial did not replace the denial confirmed longest ago")
	}
}

// Each account of a batch gets its own lookup timeout: with 64 fresh accounts and 16 lookups at a time, the later
// waves are looked up in full, not failed open because the earlier waves used up one shared deadline.
func TestDeniedAmongGivesEveryAccountItsOwnTimeout(t *testing.T) {
	g := &Gate{timeout: 150 * time.Millisecond}
	g.lookup = func(ctx context.Context, _ string, _ time.Time) (bool, error) {
		select {
		case <-time.After(100 * time.Millisecond):
			return true, nil
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}
	accounts := make([]string, 64)
	for i := range accounts {
		accounts[i] = fmt.Sprintf("usr_%02d", i)
	}
	if got := g.DeniedAmong(context.Background(), accounts, time.Now(), 64); len(got) != 64 {
		t.Fatalf("%d of 64 over-allowance accounts denied: the rest failed open on a shared deadline", len(got))
	}
}
