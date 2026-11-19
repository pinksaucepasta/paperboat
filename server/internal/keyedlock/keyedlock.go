// Package keyedlock serializes work per key within one process.
package keyedlock

import (
	"context"
	"sync"
)

// Locks serializes work per key within this process; entries are removed
// when their last holder leaves, so it never grows beyond the keys in use.
type Locks struct {
	mu    sync.Mutex
	locks map[string]*entry
}

type entry struct {
	slot chan struct{}
	refs int
}

// Lock waits for key's slot until ctx ends and returns its release.
func (k *Locks) Lock(ctx context.Context, key string) (func(), error) {
	k.mu.Lock()
	if k.locks == nil {
		k.locks = map[string]*entry{}
	}
	l := k.locks[key]
	if l == nil {
		l = &entry{slot: make(chan struct{}, 1)}
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
