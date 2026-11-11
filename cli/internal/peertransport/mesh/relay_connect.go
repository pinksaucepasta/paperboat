package mesh

import (
	"context"
	"errors"
	"sync"
	"time"

	//paperboat:allow-source-policy tailscale-import owner=peer-networking reason=upstream-engine-assembly
	"tailscale.com/tailcfg"
)

// A relay region whose connect failed is not dialed again by any handshake for relayConnectBackoff, doubling with each
// further failure up to relayConnectHold, like magicsock's per-region DERP reconnect backoff (up to 5 s,
// wgengine/magicsock/derp.go runDerpReader); a success resets it. A relay that comes back is used again within one
// step instead of after a fixed 5 s (review of b6eb2018c, P2). A region that answered a connect is used without
// another for relayConnectHold.
const (
	relayConnectBackoff = 250 * time.Millisecond
	relayConnectHold    = 5 * time.Second
)

// relayConnectTimeout bounds one connect a handshake starts.
const relayConnectTimeout = time.Second

var errRelayConnectHeld = errors.New("mesh: relay region recently failed or is being connected")

// relayConnects shares relay connects across every peer's handshake on one engine. A region that answered a connect
// within relayConnectHold is used without another; a region that failed is not used while its backoff runs (no
// packets either: a packet queued to a down region makes its carrier redial); otherwise one connect runs, and other
// handshakes skip the region until it ends. With every relay down, an engine dials each region on the backoff
// however many peers it is handshaking with.
type relayConnects struct {
	mu      sync.Mutex
	busy    map[tailcfg.DERPRegionID]bool
	until   map[tailcfg.DERPRegionID]time.Time
	healthy map[tailcfg.DERPRegionID]bool
	fails   map[tailcfg.DERPRegionID]int // consecutive failed connects
}

// ensure reports whether region may carry a handshake now, connecting it (dial) when its last result has expired.
func (r *relayConnects) ensure(ctx context.Context, region tailcfg.DERPRegionID, dial func(context.Context, tailcfg.DERPRegionID) error) error {
	r.mu.Lock()
	if r.busy == nil {
		r.busy, r.until, r.healthy = map[tailcfg.DERPRegionID]bool{}, map[tailcfg.DERPRegionID]time.Time{}, map[tailcfg.DERPRegionID]bool{}
		r.fails = map[tailcfg.DERPRegionID]int{}
	}
	if time.Now().Before(r.until[region]) {
		healthy := r.healthy[region]
		r.mu.Unlock()
		if healthy {
			return nil
		}
		return errRelayConnectHeld
	}
	if r.busy[region] {
		r.mu.Unlock()
		return errRelayConnectHeld
	}
	r.busy[region] = true
	r.mu.Unlock()
	attempt, cancel := context.WithTimeout(ctx, relayConnectTimeout)
	err := dial(attempt, region)
	cancel()
	r.mu.Lock()
	delete(r.busy, region)
	switch {
	case err == nil:
		r.until[region], r.healthy[region], r.fails[region] = time.Now().Add(relayConnectHold), true, 0
	case ctx.Err() == nil: // a dial cut short by its caller says nothing about the relay
		r.fails[region]++
		r.until[region], r.healthy[region] = time.Now().Add(relayConnectBackoffAfter(r.fails[region])), false
	}
	r.mu.Unlock()
	return err
}

// relayConnectBackoffAfter is how long a region is not dialed after its fails-th consecutive failed connect.
func relayConnectBackoffAfter(fails int) time.Duration {
	if fails > 5 {
		return relayConnectHold
	}
	return min(relayConnectBackoff<<(fails-1), relayConnectHold)
}
