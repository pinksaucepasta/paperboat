package tailnet

import (
	"testing"
	"time"

	//paperboat:allow-source-policy tailscale-import owner=peer-networking reason=regional-renewal-regression
	"tailscale.com/types/key"
)

func TestRegionalRenewalPreservesProbeAndPromotion(t *testing.T) {
	now := time.Unix(1000, 0)
	a, b := newRegionalControl(key.NewNode(), func() time.Time { return now }), newRegionalControl(key.NewNode(), func() time.Time { return now })
	node := RegionalNode{NodeID: "one", NodeGeneration: 1, ProcessEpoch: "boot"}
	scope := NetworkScope{ResourceKind: "terminal", ResourceID: "session", ResourceGeneration: 1, Capability: "terminal", Direction: "both", Port: NetworkPort, ExpiresAt: 1100}
	peers := map[key.NodePublic]NetworkPeer{b.private.Public(): {Identity: NetworkBinding{EndpointID: "host", KeyGeneration: 1}, Scopes: []NetworkScope{scope}}}
	left, right := &regionalRecovery{}, &regionalRecovery{}
	generation := left.authorityGeneration(NetworkBinding{}, peers, now.Unix())
	hostGeneration := right.authorityGeneration(NetworkBinding{}, peers, now.Unix())
	packet, pending, err := a.request(b.private.Public(), node, generation, false)
	if err != nil {
		t.Fatal(err)
	}
	ack, _ := b.handle(a.private.Public(), node, hostGeneration, packet, nil)
	// Both independently renewed configurations retain exactly the same authority.
	renewed := peers[b.private.Public()]
	renewed.Scopes = append([]NetworkScope(nil), renewed.Scopes...)
	renewed.Scopes[0].ExpiresAt += 30
	peers[b.private.Public()] = renewed
	generation = left.authorityGeneration(NetworkBinding{}, peers, now.Unix())
	hostGeneration = right.authorityGeneration(NetworkBinding{}, peers, now.Unix())
	a.handle(b.private.Public(), node, generation, ack, nil)
	select {
	case <-pending.done:
	default:
		t.Fatal("renewal discarded probe acknowledgement")
	}
	packet, pending, err = a.request(b.private.Public(), node, generation, true)
	if err != nil {
		t.Fatal(err)
	}
	promoted := 0
	ack, _ = b.handle(a.private.Public(), node, hostGeneration, packet, func() error { promoted++; return nil })
	a.handle(b.private.Public(), node, generation, ack, nil)
	select {
	case <-pending.done:
	default:
		t.Fatal("renewal discarded promotion acknowledgement")
	}
	if promoted != 1 {
		t.Fatalf("promotions=%d", promoted)
	}
}

func TestRegionalAuthorityGenerationFencesChanges(t *testing.T) {
	now := int64(1000)
	peerKey := key.NewNode().Public()
	for _, change := range []string{"self", "peer", "resource", "capability", "direction", "expiry", "removal"} {
		t.Run(change, func(t *testing.T) {
			r := &regionalRecovery{}
			self := NetworkBinding{EndpointID: "cli", KeyGeneration: 1}
			p := NetworkPeer{Identity: NetworkBinding{EndpointID: "host", KeyGeneration: 1}, Scopes: []NetworkScope{{ResourceID: "session", ResourceGeneration: 1, Capability: "terminal", Direction: "both", Port: NetworkPort, ExpiresAt: 1100}}}
			peers := map[key.NodePublic]NetworkPeer{peerKey: p}
			before := r.authorityGeneration(self, peers, now)
			switch change {
			case "self":
				self.KeyGeneration++
			case "peer":
				p.Identity.KeyGeneration++
			case "resource":
				p.Scopes[0].ResourceGeneration++
			case "capability":
				p.Scopes[0].Capability = "other"
			case "direction":
				p.Scopes[0].Direction = "inbound"
			case "expiry":
				p.Scopes[0].ExpiresAt = now
			}
			peers[peerKey] = p
			if change == "removal" {
				delete(peers, peerKey)
			}
			if r.authorityGeneration(self, peers, now) <= before {
				t.Fatal("changed authority retained proof generation")
			}
		})
	}
}
