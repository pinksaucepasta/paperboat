package tailnet

import (
	"context"
	"sync"
)

// prepareListener establishes the initial rendezvous connections. A listener
// cannot receive a dial on another authorized relay until it has connected there;
// relying on the periodic recovery sweep for this made the first dial take 5 s.
// Once connected, magicsock owns the carrier lifetime and reconnect backoff.
func (r *regionalRecovery) prepareListener(ctx context.Context) {
	nodes, _, _, err := r.snapshot()
	if err != nil {
		return
	}
	if len(nodes) > regionalShortlist {
		nodes = nodes[:regionalShortlist]
	}
	ctx, cancel := context.WithTimeout(ctx, regionalProbeTimeout)
	defer cancel()
	var workers sync.WaitGroup
	for _, node := range nodes {
		workers.Add(1)
		go func() {
			defer workers.Done()
			_ = r.engine.PrepareRelay(ctx, regionalID(node.NodeID))
		}()
	}
	workers.Wait()
}
