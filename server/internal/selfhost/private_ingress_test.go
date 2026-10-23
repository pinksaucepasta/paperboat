package selfhost

import (
	"net/netip"
	"testing"
)

// Staging on a LAN (Lane W, pbvm4) claims a node at a private address only when the service allows it;
// loopback, link-local and documentation ranges stay refused either way.
func TestPrivateIngressIsAcceptedOnlyWhenAllowed(t *testing.T) {
	var service Service
	private := []string{"192.168.31.149", "10.0.0.7", "172.16.4.2", "fd00::1"}
	never := []string{"127.0.0.1", "169.254.1.1", "::1", "192.0.2.1", "0.0.0.1"}
	for _, text := range private {
		if service.ingressIP(netip.MustParseAddr(text)) {
			t.Fatalf("%s accepted without the staging switch", text)
		}
	}
	service.AllowPrivateIngress()
	for _, text := range private {
		if !service.ingressIP(netip.MustParseAddr(text)) || !service.validHost(text) {
			t.Fatalf("%s refused with the staging switch", text)
		}
	}
	for _, text := range never {
		if service.ingressIP(netip.MustParseAddr(text)) {
			t.Fatalf("%s accepted with the staging switch", text)
		}
	}
}
