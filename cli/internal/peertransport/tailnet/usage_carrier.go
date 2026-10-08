package tailnet

import (
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
	r.usagePeers = current
}

// usageObservedCarrier observes the carrier chosen after Magicsock's routing,
// including reverse routes that differ from a peer's advertised home region.
// Embedding preserves cancellation, reconnect and bounded carrier I/O behavior.
type usageObservedCarrier struct {
	magicsock.DERPCarrier
	authority *relayAuthority
	node      RegionalNode
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
func (c *usageObservedCarrier) RecvDetail() (derp.ReceivedMessage, int, error) {
	message, generation, err := c.DERPCarrier.RecvDetail()
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
	observation := c.authority.usagePeers[peer]
	if observation == nil {
		return
	}
	sample := relayUsageSample{node: c.node, at: at}
	if receive {
		observation.receive = sample
	} else {
		observation.send = sample
	}
}
