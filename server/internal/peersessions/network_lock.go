package peersessions

import (
	"context"
	"sync"
)

// keyedLocks serializes work per key within this process; entries are removed
// when their last holder leaves, so it never grows beyond the keys in use.
type keyedLocks struct {
	mu    sync.Mutex
	locks map[string]*keyedLock
}

type keyedLock struct {
	slot chan struct{}
	refs int
}

func (k *keyedLocks) lock(ctx context.Context, key string) (func(), error) {
	k.mu.Lock()
	if k.locks == nil {
		k.locks = map[string]*keyedLock{}
	}
	l := k.locks[key]
	if l == nil {
		l = &keyedLock{slot: make(chan struct{}, 1)}
		k.locks[key] = l
	}
	l.refs++
	k.mu.Unlock()
	releaseRef := func() {
		k.mu.Lock()
		if l.refs--; l.refs == 0 {
			delete(k.locks, key)
		}
		k.mu.Unlock()
	}
	// Like tailscale/syncs.Semaphore.AcquireContext, waiting never outlives the request.
	select {
	case l.slot <- struct{}{}:
		if err := ctx.Err(); err != nil {
			<-l.slot
			releaseRef()
			return nil, err
		}
		return func() { <-l.slot; releaseRef() }, nil
	case <-ctx.Done():
		releaseRef()
		return nil, ctx.Err()
	}
}
