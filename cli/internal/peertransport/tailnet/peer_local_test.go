package tailnet

import (
	"net"
	"net/netip"
	"testing"
)

// Different hosts can advertise the same private Docker/LAN addresses. An address match must not
// skip regional relay preparation for a remote machine (dadape -> hp, 2026-10-27).
func TestPeerLocalityUsesMachineIdentityDespiteOverlappingAddresses(t *testing.T) {
	addresses, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatal(err)
	}
	var overlap netip.Addr
	for _, address := range addresses {
		prefix, err := netip.ParsePrefix(address.String())
		if err == nil && prefix.Addr().IsPrivate() && !prefix.Addr().IsLoopback() {
			overlap = prefix.Addr()
			break
		}
	}
	if !overlap.IsValid() {
		t.Fatal("fixture needs a private interface address")
	}
	a, c, signer, _ := networkTestAuthority(t)
	c.Peers[0].Endpoints = []string{netip.AddrPortFrom(overlap, 41642).String()}
	if err := a.Apply(t.Context(), networkToken(t, signer, c)); err != nil {
		t.Fatal(err)
	}
	peer := c.Peers[0].Identity
	a.options.LocalMachineID = "machine_other"
	if a.PeerIsLocal(peer.EndpointID) {
		t.Fatal("overlapping private address made a different machine local")
	}
	a.options.LocalMachineID = peer.MachineID
	if !a.PeerIsLocal(peer.EndpointID) {
		t.Fatal("this machine's own runtime is not local")
	}
	a.options.LocalMachineID = ""
	if a.PeerIsLocal(peer.EndpointID) || a.PeerIsLocal("endpoint_unknown") {
		t.Fatal("unknown machine identity was inferred from an address")
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
