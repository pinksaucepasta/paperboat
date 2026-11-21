package selfhost

import "testing"

// A self-hosted component's declared size has no ceiling (relay or tunnel); only a missing size is refused.
func TestComponentCapacityHasNoCeiling(t *testing.T) {
	config := func(capability string, capacity int64) Config {
		return Config{Name: "native-test", EndpointHost: "127.0.0.1", Listen: "127.0.0.1:18443", Components: []Component{{Capability: capability, TCPPort: 7274, QUICPort: 7275, Region: "test", FailureDomain: "test-host", CapacityLimit: capacity}}}
	}
	for _, accepted := range []Config{config("relay", 4096), config("relay", 100000), config("tunnel", 257), config("tunnel", 100000)} {
		if err := validateConfig(accepted); err != nil {
			t.Fatalf("%s capacity %d refused: %v", accepted.Components[0].Capability, accepted.Components[0].CapacityLimit, err)
		}
	}
	if err := validateConfig(config("relay", 0)); err == nil {
		t.Fatal("a component with no capacity was accepted")
	}
}
