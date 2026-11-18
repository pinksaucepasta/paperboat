package nativeprivateaccess

import (
	"context"
	"sync"
)

// sessionGate serializes the grants of one CLI session inside this process before they take a database
// connection. The grants already queue on a row lock of the session (see resolveMachineService), but each queued
// grant held a pooled connection while it waited: a burst of 300 grants from one session used the whole pool, every
// other request timed out acquiring a connection, and the queued grants themselves hit the 3 s acquire bound
// (TestNativePrivateGrantBurstSucceedsPostgres, 260 of 300 failed under load). Waiting here costs a goroutine, is
// bounded by the caller's context, and the row lock still orders grants across server processes.
type sessionGate struct {
	mu    sync.Mutex
	gates map[string]*gateSlot
}

type gateSlot struct {
	token   chan struct{}
	waiters int
}

// enter returns once key is free, or ctx's error; the returned function releases it and must be called exactly once.
func (g *sessionGate) enter(ctx context.Context, key string) (func(), error) {
	g.mu.Lock()
	if g.gates == nil {
		g.gates = map[string]*gateSlot{}
	}
	slot := g.gates[key]
	if slot == nil {
		slot = &gateSlot{token: make(chan struct{}, 1)}
		g.gates[key] = slot
	}
	slot.waiters++
	g.mu.Unlock()
	leave := func() {
		g.mu.Lock()
		if slot.waiters--; slot.waiters == 0 {
			delete(g.gates, key)
		}
		g.mu.Unlock()
	}
	select {
	case slot.token <- struct{}{}:
		return func() { <-slot.token; leave() }, nil
	case <-ctx.Done():
		leave()
		return nil, ctx.Err()
	}
}
