package machineguard

import (
	"context"
	"errors"
	"testing"
)

// The guard account is looked up until it is found, then never again.
func TestGuardIdentityIsLookedUpOnceFound(t *testing.T) {
	calls := 0
	lookup := cacheIdentity(func(context.Context) (uint32, uint32, error) {
		calls++
		if calls == 1 {
			return 0, 0, errors.New("account missing")
		}
		return 70, 71, nil
	})
	if _, _, err := lookup(t.Context()); err == nil {
		t.Fatal("a failed lookup was cached as found")
	}
	for range 3 {
		if uid, gid, err := lookup(t.Context()); err != nil || uid != 70 || gid != 71 {
			t.Fatalf("got %d %d %v", uid, gid, err)
		}
	}
	if calls != 2 {
		t.Fatalf("lookup ran %d times; want 2 (one failure, one success)", calls)
	}
}
