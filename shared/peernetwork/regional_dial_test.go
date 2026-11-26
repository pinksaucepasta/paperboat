package peernetwork

import "testing"

// A relay is dialed at the address the server verified (a self-hosted one may be private), else by name.
func TestDialHostPrefersTheVerifiedAddress(t *testing.T) {
	for _, c := range []struct{ v4, v6, want string }{
		{"152.67.0.60", "2001:db8::1", "152.67.0.60"},
		{"", "2a01:4f8::1", "2a01:4f8::1"},
		{"10.0.0.1", "", "10.0.0.1"}, {"127.0.0.1", "", "edge.example.test"},
		{"not-an-ip", "", "edge.example.test"},
		{"", "", "edge.example.test"},
	} {
		if got := (RegionalNode{EndpointHost: "edge.example.test", EndpointIPv4: c.v4, EndpointIPv6: c.v6}).DialHost(); got != c.want {
			t.Errorf("v4 %q v6 %q: DialHost %q, want %q", c.v4, c.v6, got, c.want)
		}
	}
}
