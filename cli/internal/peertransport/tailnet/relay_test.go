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
