package selfhost

import (
	"testing"

	"github.com/pinksaucepasta/paperboat/shared/relaylimits"
)

// A self-hosted relay is sized like a managed one, up to the shared ceiling; a tunnel keeps 256.
func TestRelayCapacityFollowsTheSharedCeiling(t *testing.T) {
	config := func(capability string, capacity int64) Config {
		return Config{Name: "native-test", EndpointHost: "127.0.0.1", Listen: "127.0.0.1:18443", Components: []Component{{Capability: capability, TCPPort: 27445, QUICPort: 27446, Region: "test", FailureDomain: "test-host", CapacityLimit: capacity}}}
	}
	for _, accepted := range []Config{config("relay", 4096), config("relay", relaylimits.MaxConnectionsPerNode), config("tunnel", 256)} {
		if err := validateConfig(accepted); err != nil {
			t.Fatalf("%s capacity %d refused: %v", accepted.Components[0].Capability, accepted.Components[0].CapacityLimit, err)
		}
	}
	for _, refused := range []Config{config("relay", relaylimits.MaxConnectionsPerNode+1), config("tunnel", 257)} {
		if err := validateConfig(refused); err == nil {
			t.Fatalf("%s capacity %d accepted", refused.Components[0].Capability, refused.Components[0].CapacityLimit)
		}
	}
}
