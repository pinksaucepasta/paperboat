package operatorinfra

import "testing"

// A relay node's registered size has no ceiling: an operator registers the real node (any positive size, now only informational).
func TestRelayNodeCapacityHasNoCeiling(t *testing.T) {
	relay := func(capacity int64) NodeMutation {
		return NodeMutation{RegionID: "eu-test", EdgePool: "managed-eu", FailureDomain: "zone-a", EndpointHost: "relay.example.test",
			EndpointTCPPort: 443, EndpointQUICPort: 8443, Roles: []string{"relay"}, Transports: []string{"derp_quic", "derp_wss"}, CapacityLimit: capacity}
	}
	for capacity, want := range map[int64]bool{1: true, 4096: true, 16385: true, 1 << 20: true, 0: false} {
		if got := validNode(relay(capacity)); got != want {
			t.Errorf("relay capacity %d valid = %v, want %v", capacity, got, want)
		}
	}
}
