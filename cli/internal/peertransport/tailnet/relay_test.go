package tailnet

import (
	"errors"
	"testing"
)

func TestDiscoveryRegistrationFencesKeyRotation(t *testing.T) {
	authority, cfg, signer, _ := networkTestAuthority(t)
	first, err := authority.discoveryPublicKey(cfg.Self.WireGuardPublicKey)
	if err != nil || first == "" {
		t.Fatal("missing staged discovery identity", err)
	}
	if err = authority.Apply(t.Context(), networkToken(t, signer, cfg)); err != nil {
		t.Fatal(err)
	}
	committed, err := authority.DiscoveryPublicKey()
	if err != nil || committed != first {
		t.Fatal("commit changed discovery identity", err)
	}
	replacement, _, err := authority.PrepareKey(true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = authority.discoveryPublicKey(cfg.Self.WireGuardPublicKey); !errors.Is(err, ErrStaleAuthority) {
		t.Fatalf("stale registration accepted after rotation: %v", err)
	}
	rotated, err := authority.discoveryPublicKey(replacement)
	if err != nil || rotated == first {
		t.Fatal("rotation did not replace discovery identity", err)
	}
}

func TestRegionalRelayAdvertisesSTUNOnWSSPort(t *testing.T) {
	region := regionForNode(RegionalNode{NodeID: "relay-1", NodeGeneration: 1, Region: "in", EndpointHost: "relay.example.test", EndpointTCPPort: 28443, EndpointQUICPort: 28444})
	if len(region.Nodes) != 1 || region.Nodes[0].STUNPort != 28443 || region.Nodes[0].DERPPort != 28444 {
		t.Fatalf("incorrect regional relay ports: %+v", region.Nodes)
	}
}

// The region carries the server's verified address, so magicsock, netcheck and STUN dial it without a lookup.
func TestRegionalRelayCarriesTheVerifiedAddress(t *testing.T) {
	region := regionForNode(RegionalNode{NodeID: "relay-1", Region: "in", EndpointHost: "relay.example.test", EndpointIPv4: "152.67.0.60", EndpointIPv6: "2001:db8::1", EndpointTCPPort: 28443, EndpointQUICPort: 28444})
	if n := region.Nodes[0]; n.IPv4 != "152.67.0.60" || n.IPv6 != "2001:db8::1" || n.HostName != "relay.example.test" {
		t.Fatalf("region node %+v", n)
	}
}

// A bad address never reaches the DERP map, and a node without a port has no STUN (0 would mean 3478).
func TestRegionalRelayDropsABadAddressAndUnsetSTUN(t *testing.T) {
	n := regionForNode(RegionalNode{NodeID: "relay-1", EndpointHost: "relay.example.test", EndpointIPv4: "10.0.0.1", EndpointIPv6: "fe80::1", EndpointQUICPort: 28444}).Nodes[0]
	if n.IPv4 != "" || n.IPv6 != "" || n.STUNPort != -1 {
		t.Fatalf("region node %+v", n)
	}
}
