package operatorinfra

import (
	"testing"

	"github.com/pinksaucepasta/paperboat/shared/relaylimits"
)

// A relay node's registered capacity is bounded by the shared ceiling the relay also enforces,
// not by a fixed 256: an operator registers real node sizes (thousands of connections).
func TestRelayNodeCapacityIsBoundedByTheSharedCeiling(t *testing.T) {
	relay := func(capacity int64) NodeMutation {
		return NodeMutation{RegionID: "eu-test", EdgePool: "managed-eu", FailureDomain: "zone-a", EndpointHost: "relay.example.test",
			EndpointTCPPort: 443, EndpointQUICPort: 8443, Roles: []string{"relay"}, Transports: []string{"derp_quic", "derp_wss"}, CapacityLimit: capacity}
	}
	for capacity, want := range map[int64]bool{1: true, 257: true, 4096: true, relaylimits.MaxConnectionsPerNode: true,
		relaylimits.MaxConnectionsPerNode + 1: false, 0: false} {
		if got := validNode(relay(capacity)); got != want {
			t.Errorf("relay capacity %d valid = %v, want %v", capacity, got, want)
		}
	}
}
