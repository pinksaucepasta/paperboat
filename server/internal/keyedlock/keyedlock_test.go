package keyedlock

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestKeyedLocksSerializeOneKeyAndForgetIt(t *testing.T) {
	var locks Locks
	var inside, maximum int32
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			unlock, err := locks.Lock(t.Context(), "endpoint")
			if err != nil {
				t.Error(err)
				return
			}
			if n := atomic.AddInt32(&inside, 1); n > atomic.LoadInt32(&maximum) {
				atomic.StoreInt32(&maximum, n)
			}
			time.Sleep(2 * time.Millisecond)
			atomic.AddInt32(&inside, -1)
			unlock()
		}()
	}
	wg.Wait()
	if maximum != 1 {
		t.Fatalf("%d builds of one endpoint ran at once", maximum)
	}
	if len(locks.locks) != 0 {
		t.Fatal("lock entries outlived their holders")
	}
}

// A waiter whose context ends leaves without keeping a reference to the key.
func TestLockHonorsCancellation(t *testing.T) {
	var locks Locks
	unlock, err := locks.Lock(t.Context(), "key")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := locks.Lock(ctx, "key"); err == nil {
		t.Fatal("a canceled waiter got the lock")
	}
	locks.mu.Lock()
	refs := locks.locks["key"].refs
	locks.mu.Unlock()
	if refs != 1 {
		t.Fatalf("%d references after a canceled waiter left, want 1", refs)
	}
	unlock()
}
