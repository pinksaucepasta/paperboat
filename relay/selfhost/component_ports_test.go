package selfhost

import "testing"

// A component may not take the claim listener's port (default or custom) nor the edge's loopback ports.
func TestComponentPortsRefuseTheClaimAndLoopbackPorts(t *testing.T) {
	config := func(listen string, tcp, quic int) Config {
		return Config{Name: "native-test", EndpointHost: "127.0.0.1", Listen: listen, Components: []Component{{Capability: "tunnel", TCPPort: tcp, QUICPort: quic, Region: "test", FailureDomain: "test-host"}}}
	}
	if err := validateConfig(config("[::]:7277", 7272, 7273)); err != nil {
		t.Fatalf("default ports refused: %v", err)
	}
	for _, refused := range []Config{config("[::]:7277", 7277, 7273), config("[::]:7272", 7272, 7273), config("[::]:7277", 7278, 7273), config("[::]:7277", 7272, 7279)} {
		if err := validateConfig(refused); err == nil {
			t.Fatalf("listen %s tcp %d quic %d was accepted", refused.Listen, refused.Components[0].TCPPort, refused.Components[0].QUICPort)
		}
	}
}

// The claim listen port must be a real number: 0 would bind a random port and a service name would be
// read differently by the firewall rules than by the listener.
func TestClaimListenPortMustBeANumber(t *testing.T) {
	for _, listen := range []string{"[::]:0", "[::]:http", "[::]:65536", "[::]:"} {
		c := Config{Name: "native-test", EndpointHost: "127.0.0.1", Listen: listen, Components: []Component{{Capability: "tunnel", TCPPort: 7272, QUICPort: 7273, Region: "test", FailureDomain: "test-host"}}}
		if err := validateConfig(c); err == nil {
			t.Fatalf("claim listen %q accepted", listen)
		}
	}
}
