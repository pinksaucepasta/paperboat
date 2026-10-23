package mesh

import (
	"testing"
	"time"

	//paperboat:allow-source-policy tailscale-import owner=peer-networking reason=engine-unit-test
	"tailscale.com/types/key"
)

// Metered connections asked for the peer's path before and after every read and write, and each answer
// built magicsock's status of every peer. The path is now computed at most once per usagePathTTL per peer.
func TestUsagePathIsComputedOncePerTTLPerPeer(t *testing.T) {
	var cache usagePathCache
	computes := 0
	direct := func() UsagePath { computes++; return UsagePath{Mode: "direct"} }
	a, b := key.NewNode().Public(), key.NewNode().Public()
	now := time.Unix(1_000, 0)
	for range 1000 {
		if got := cache.get(a, now, direct); got.Mode != "direct" {
			t.Fatalf("path = %+v", got)
		}
	}
	if computes != 1 {
		t.Fatalf("%d status walks for 1,000 reads within the TTL, want 1", computes)
	}
	cache.get(b, now, direct)
	cache.get(a, now.Add(usagePathTTL), direct)
	if computes != 3 {
		t.Fatalf("computes = %d, want one more per other peer and per expired entry", computes)
	}
	for i := range maxUsagePaths + 10 {
		cache.get(key.NewNode().Public(), now.Add(time.Duration(i)*usagePathTTL), direct)
	}
	if len(cache.paths) > maxUsagePaths {
		t.Fatalf("cache holds %d peers, bound %d", len(cache.paths), maxUsagePaths)
	}
}
