package peernetwork

import "testing"

// A relay is dialed at the address the server verified, else by name.
func TestDialHostPrefersTheVerifiedAddress(t *testing.T) {
	for _, c := range []struct{ v4, v6, want string }{
		{"152.67.0.60", "2001:db8::1", "152.67.0.60"},
		{"", "2a01:4f8::1", "2a01:4f8::1"},
		{"10.0.0.1", "", "edge.example.test"},
		{"not-an-ip", "", "edge.example.test"},
		{"", "", "edge.example.test"},
	} {
		if got := (RegionalNode{EndpointHost: "edge.example.test", EndpointIPv4: c.v4, EndpointIPv6: c.v6}).DialHost(); got != c.want {
			t.Errorf("v4 %q v6 %q: DialHost %q, want %q", c.v4, c.v6, got, c.want)
		}
	}
}

// An address unfit to dial is left out, so the name is looked up; a self-hosted relay may be private.
func TestUnfitAddressesAreLeftOut(t *testing.T) {
	for _, bad := range []struct{ v4, v6 string }{
		{"0.0.0.0", "::"}, {"127.0.0.1", "::1"}, {"169.254.1.1", "fe80::1"}, {"10.0.0.1", "fd00::1"},
		{"100.64.0.1", ""}, {"1.2.3", "zz::"}, {"2001:db8::1", "152.67.0.60"}, {"", "::ffff:152.67.0.60"},
		{"", "fe80::1%eth0"},
	} {
		n := RegionalNode{EndpointHost: "edge.example.test", EndpointIPv4: bad.v4, EndpointIPv6: bad.v6}
		if n.IPv4() != "" || n.IPv6() != "" || n.DialHost() != "edge.example.test" {
			t.Errorf("%+v: kept v4 %q v6 %q", bad, n.IPv4(), n.IPv6())
		}
	}
	own := RegionalNode{Hosting: HostingSelfHosted, EndpointIPv4: "10.0.0.1", EndpointIPv6: "fd00::1"}
	if own.IPv4() != "10.0.0.1" || own.IPv6() != "fd00::1" {
		t.Fatalf("self-hosted private dropped: %q %q", own.IPv4(), own.IPv6())
	}
}
