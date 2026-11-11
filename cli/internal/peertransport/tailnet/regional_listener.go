package tailnet

import (
	"context"
	"sync"
	"sync/atomic"
)

// prepareListener establishes the initial rendezvous connections. A listener
// cannot receive a dial on another authorized relay until it has connected there;
// relying on the periodic recovery sweep for this made the first dial take 5 s.
// Once connected, magicsock owns the carrier lifetime and reconnect backoff.
// A listener that connected to no relay (restored before its relays were known, or every relay silent) has no
// relay presence and no STUN endpoint, and nothing else would retry it: it becomes recovery demand, swept with
// per-relay backoff until a relay connects (Tailscale keeps redialing its home DERP, wgengine/magicsock/derp.go).
func (r *regionalRecovery) prepareListener(lifetime context.Context) {
	nodes, _, _, _ := r.snapshot()
	if len(nodes) > regionalShortlist {
		nodes = nodes[:regionalShortlist]
	}
	ctx, cancel := context.WithTimeout(lifetime, regionalProbeTimeout)
	defer cancel()
	var workers sync.WaitGroup
	var prepared atomic.Bool
	for _, node := range nodes {
		workers.Add(1)
		go func() {
			defer workers.Done()
			if r.engine.PrepareRelay(ctx, regionalID(node.NodeID)) == nil {
				prepared.Store(true)
			}
		}()
	}
	workers.Wait()
	if !prepared.Load() && lifetime.Err() == nil && r.repair.CompareAndSwap(false, true) {
		r.poke()
	}
}
