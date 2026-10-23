package mesh

import (
	"fmt"
	"net/netip"
	"testing"

	//paperboat:allow-source-policy tailscale-import owner=peer-networking reason=engine-unit-test
	"tailscale.com/tailcfg"
	//paperboat:allow-source-policy tailscale-import owner=peer-networking reason=engine-unit-test
	"tailscale.com/types/key"
)

func indexedClient(i int) (key.NodePublic, *tailcfg.Node, netip.Addr) {
	k := key.NewNode().Public()
	addr := netip.MustParseAddr(fmt.Sprintf("fd7a:115c:a1e0::%x", i+2))
	return k, &tailcfg.Node{Key: k, Addresses: []netip.Prefix{pfxOf(addr)}, AllowedIPs: []netip.Prefix{pfxOf(addr)}}, addr
}

// peerByIP runs for every outbound packet. It scanned every admitted client under the backend
// lock, so a host many peers talk to paid O(peers) per packet; the address index keeps it O(1)
// and in step with every client change.
func TestClientAddressIndexFollowsEveryClientChange(t *testing.T) {
	b := &locoBackend{}
	k1, n1, a1 := indexedClient(1)
	k2, n2, a2 := indexedClient(2)
	b.setClientLocked(k1, n1)
	b.setClientLocked(k2, n2)
	if got, ok := b.peerByIP(a1); !ok || got != k1 {
		t.Fatalf("peerByIP(%v) = %v %v, want first client", a1, got, ok)
	}
	// A relay-region update replaces the node and keeps its address.
	moved := n2.Clone()
	moved.HomeDERP = 7
	b.setClientLocked(k2, moved)
	if got, ok := b.peerByIP(a2); !ok || got != k2 || b.clients[k2].HomeDERP != 7 {
		t.Fatalf("replaced client lost its address index: %v %v", got, ok)
	}
	// A key that is admitted again under a new address drops the old one.
	_, renumbered, a3 := indexedClient(3)
	b.setClientLocked(k1, renumbered)
	if _, ok := b.peerByIP(a1); ok {
		t.Fatal("old address still routes to a renumbered client")
	}
	if got, ok := b.peerByIP(a3); !ok || got != k1 {
		t.Fatalf("new address not indexed: %v %v", got, ok)
	}
	b.deleteClientLocked(k1)
	b.deleteClientLocked(k2)
	if _, ok := b.peerByIP(a2); ok || len(b.clientByAddr) != 0 || len(b.clients) != 0 {
		t.Fatalf("removed clients still indexed: clients=%d index=%d", len(b.clients), len(b.clientByAddr))
	}
}

func BenchmarkPeerByIPWith10kClients(b *testing.B) {
	backend := &locoBackend{}
	var last netip.Addr
	for i := range 10_000 {
		k, n, addr := indexedClient(i)
		backend.setClientLocked(k, n)
		last = addr
	}
	b.ReportAllocs()
	for b.Loop() {
		if _, ok := backend.peerByIP(last); !ok {
			b.Fatal("lookup failed")
		}
	}
}
