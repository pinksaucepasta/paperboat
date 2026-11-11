package mesh

import (
	"context"
	"errors"
	"sync"
	"time"

	//paperboat:allow-source-policy tailscale-import owner=peer-networking reason=upstream-engine-assembly
	"tailscale.com/tailcfg"
)

// relayConnectHold is how long a relay region whose connect failed is not dialed again by any handshake, like
// magicsock's per-region DERP reconnect backoff (up to 5 s, wgengine/magicsock/derp.go runDerpReader).
const relayConnectHold = 5 * time.Second

// relayConnectTimeout bounds one connect a handshake starts.
const relayConnectTimeout = time.Second

var errRelayConnectHeld = errors.New("mesh: relay region recently failed or is being connected")

// relayConnects shares relay connects across every peer's handshake on one engine. A region that answered a connect
// within relayConnectHold is used without another; a region that failed one is not used for relayConnectHold (no
// packets either: a packet queued to a down region makes its carrier redial); otherwise one connect runs, and other
// handshakes skip the region until it ends. With every relay down, an engine dials each region about once per
// hold however many peers it is handshaking with.
type relayConnects struct {
	mu      sync.Mutex
	busy    map[tailcfg.DERPRegionID]bool
	until   map[tailcfg.DERPRegionID]time.Time
	healthy map[tailcfg.DERPRegionID]bool
}

// ensure reports whether region may carry a handshake now, connecting it (dial) when its last result has expired.
func (r *relayConnects) ensure(ctx context.Context, region tailcfg.DERPRegionID, dial func(context.Context, tailcfg.DERPRegionID) error) error {
	r.mu.Lock()
	if r.busy == nil {
		r.busy, r.until, r.healthy = map[tailcfg.DERPRegionID]bool{}, map[tailcfg.DERPRegionID]time.Time{}, map[tailcfg.DERPRegionID]bool{}
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
	if err == nil || ctx.Err() == nil { // a dial cut short by its caller says nothing about the relay
		r.until[region], r.healthy[region] = time.Now().Add(relayConnectHold), err == nil
	}
	r.mu.Unlock()
	return err
}
