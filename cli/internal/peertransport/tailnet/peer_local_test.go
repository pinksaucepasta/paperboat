package tailnet

import (
	"net/netip"
	"testing"
)

// The daemon dialing its own machine's host runtime (inspecting a local preview) must not bind regional
// recovery to a relay pair with itself: a peer whose signed endpoint is a local interface address is local.
func TestPeerWithALocalEndpointIsLocal(t *testing.T) {
	a, c, signer, _ := networkTestAuthority(t)
	c.Peers[0].Endpoints = []string{"192.0.2.7:41641"}
	if err := a.Apply(t.Context(), networkToken(t, signer, c)); err != nil {
		t.Fatal(err)
	}
	previous := localAddresses
	t.Cleanup(func() { localAddresses = previous })
	peer := c.Peers[0].Identity.EndpointID
	localAddresses = func() []netip.Addr { return []netip.Addr{netip.MustParseAddr("192.0.2.7")} }
	if !a.PeerIsLocal(peer) {
		t.Fatal("peer whose endpoint is a local address is not local")
	}
	localAddresses = func() []netip.Addr { return []netip.Addr{netip.MustParseAddr("192.0.2.8")} }
	if a.PeerIsLocal(peer) {
		t.Fatal("peer on another host is local")
	}
	if a.PeerIsLocal("endpoint_unknown") {
		t.Fatal("unknown peer is local")
	}
}

// A self-dial is local by machine ID even while the host's endpoints are missing from the configuration:
// a self-dial in that window bound regional recovery to the machine itself (hp 2026-10-27).
func TestOwnMachineIsLocalWithoutEndpoints(t *testing.T) {
	a, c, signer, _ := networkTestAuthority(t)
	c.Peers[0].Endpoints = nil
	if err := a.Apply(t.Context(), networkToken(t, signer, c)); err != nil {
		t.Fatal(err)
	}
	peer := c.Peers[0].Identity
	if a.PeerIsLocal(peer.EndpointID) {
		t.Fatal("peer without endpoints is local while this machine is not enrolled")
	}
	a.options.LocalMachineID = peer.MachineID
	if !a.PeerIsLocal(peer.EndpointID) {
		t.Fatal("this machine's own host runtime is not local without endpoints")
	}
	a.options.LocalMachineID = "machine_other"
	if a.PeerIsLocal(peer.EndpointID) {
		t.Fatal("another machine is local")
	}
}
