package tailnet

import (
	"context"
	"errors"
	"sync/atomic"
	"time"
	//paperboat:allow-source-policy tailscale-import owner=peer-networking reason=peer-path-accounting
	"tailscale.com/derp"
	//paperboat:allow-source-policy tailscale-import owner=peer-networking reason=peer-path-accounting
	"tailscale.com/types/key"
	//paperboat:allow-source-policy tailscale-import owner=peer-networking reason=peer-path-accounting
	"tailscale.com/wgengine/magicsock"
)

const relayUsageFreshness = time.Second

type relayUsageSample struct {
	node RegionalNode
	at   time.Time
}
type relayUsageObservation struct{ send, receive relayUsageSample }

// updateUsagePeersLocked follows current admission instead of accumulating
// every peer seen across configuration refreshes. relayAuthority.mu is held.
func (r *relayAuthority) updateUsagePeersLocked(peers []NetworkPeer) {
	current := make(map[key.NodePublic]*relayUsageObservation, len(peers))
	for _, peer := range peers {
		public, err := publicKey(peer.Identity.WireGuardPublicKey)
		if err != nil {
			continue // Invalid network bindings cannot be admitted by Apply.
		}
		current[public] = r.usagePeers[public]
		if current[public] == nil {
			current[public] = &relayUsageObservation{}
		}
	}
	for _, observations := range r.generationUsage {
		for peer := range observations {
			if current[peer] == nil {
				delete(observations, peer)
			}
		}
		for peer := range current {
			if observations[peer] == nil {
				observations[peer] = &relayUsageObservation{}
			}
		}
	}
	r.usagePeers = current
}

// usageObservedCarrier observes the carrier chosen after Magicsock's routing,
// including reverse routes that differ from a peer's advertised home region.
// Embedding preserves cancellation, reconnect and bounded carrier I/O behavior.
type usageObservedCarrier struct {
	magicsock.DERPCarrier
	authority *relayAuthority
	node      RegionalNode
	namespace [32]byte
	closed    atomic.Bool
}

// A lost listening path is real recovery demand even before a session exists.
// Normal idle grant renewals do not wake recovery; a broken carrier does.
func (c *usageObservedCarrier) lostPath() {
	if c.closed.Load() {
		return
	}
	c.authority.mu.Lock()
	r := c.authority.recovery
	c.authority.mu.Unlock()
	if r != nil && r.repair.CompareAndSwap(false, true) {
		r.poke()
	}
}

func (c *usageObservedCarrier) Connect(ctx context.Context) error {
	err := c.DERPCarrier.Connect(ctx)
	if err != nil && ctx.Err() == nil {
		c.lostPath()
	}
	return err
}

func (c *usageObservedCarrier) Close() error {
	c.closed.Store(true) // an intentional idle reap is not a lost path
	return c.DERPCarrier.Close()
}

func (c *usageObservedCarrier) Transport() string {
	if reporter, ok := c.DERPCarrier.(interface{ Transport() string }); ok {
		return reporter.Transport()
	}
	return ""
}

func (c *usageObservedCarrier) Send(peer key.NodePublic, data []byte) error {
	err := c.DERPCarrier.Send(peer, data)
	if err == nil && wireGuardData(data) {
		c.record(peer, false, time.Now())
	}
	return err
}
func (c *usageObservedCarrier) SendControl(peer key.NodePublic, data []byte) error {
	if control, ok := c.DERPCarrier.(magicsock.DERPControlSender); ok {
		return control.SendControl(peer, data)
	}
	return c.DERPCarrier.Send(peer, data)
}

// relayRefusedRead is a relay refusal seen while reading. magicsock stops a region's reader for good on a fatal error
// and then refuses to prepare that region again, so a grant refused once (a revocation floor the next grant is
// above) left the relay dead until a restart. The carrier keeps the refusal for the grant it answered and lets a
// fresh grant through (derpquic refused); magicsock retries with its own backoff meanwhile. No Unwrap: magicsock
// must not see the fatal cause.
type relayRefusedRead struct{ cause error }

func (e relayRefusedRead) Error() string {
	return "the relay refused this machine's grant; waiting for a fresh grant: " + e.cause.Error()
}

func (c *usageObservedCarrier) RecvDetail() (derp.ReceivedMessage, int, error) {
	message, generation, err := c.DERPCarrier.RecvDetail()
	if err != nil {
		c.lostPath()
		var fatal interface{ Fatal() bool }
		if errors.As(err, &fatal) && fatal.Fatal() {
			err = relayRefusedRead{cause: err}
		}
	}
	if packet, ok := message.(derp.ReceivedPacket); err == nil && ok && wireGuardData(packet.Data) {
		c.record(packet.Source, true, time.Now())
	}
	return message, generation, err
}
func wireGuardData(data []byte) bool {
	return len(data) >= 4 && data[0] == 4 && data[1] == 0 && data[2] == 0 && data[3] == 0
}
func (c *usageObservedCarrier) record(peer key.NodePublic, receive bool, at time.Time) {
	c.authority.mu.Lock()
	defer c.authority.mu.Unlock()
	observations := c.authority.usagePeers
	if c.namespace != ([32]byte{}) {
		observations = c.authority.generationUsage[c.namespace]
	}
	observation := observations[peer]
	if observation == nil {
		return
	}
	sample := relayUsageSample{node: c.node, at: at}
	if receive {
		observation.receive = sample
	} else {
		observation.send = sample
	}
	if signal, ok := c.authority.hostedData[peer]; ok && !c.node.SelfHosted() {
		close(signal)
		delete(c.authority.hostedData, peer)
	}
}

// hostedRelayData returns a channel closed by the next WireGuard data to or from peer that crosses a
// relay Paperboat hosts. The caller must register it before it reads the path, so no packet slips by.
func (r *relayAuthority) hostedRelayData(peer key.NodePublic) <-chan struct{} {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.hostedData == nil {
		r.hostedData = make(map[key.NodePublic]chan struct{})
	}
	signal, ok := r.hostedData[peer]
	if !ok {
		signal = make(chan struct{})
		r.hostedData[peer] = signal
	}
	return signal
}

// hostedRelayRecent reports whether WireGuard data to or from peer crossed a hosted relay within
// relayUsageFreshness. relayAuthority.mu is held.
func (r *relayAuthority) hostedRelayRecentLocked(peer key.NodePublic, now time.Time) bool {
	observation := r.usagePeers[peer]
	if observation == nil {
		return false
	}
	for _, sample := range []relayUsageSample{observation.send, observation.receive} {
		if !sample.at.IsZero() && now.Sub(sample.at) <= relayUsageFreshness && !sample.node.SelfHosted() {
			return true
		}
	}
	return false
}
