package derpquic

import (
	"fmt"
	"testing"
)

// Every forwarded datagram checks its pair's reciprocal scopes; with the grants' peer lists scanned,
// that cost grew with the account's machines.
func BenchmarkPairCheckWithMergedPeers(b *testing.B) {
	scope := func(direction string) []Scope {
		return []Scope{{ResourceKind: "machine_access", ResourceID: "grant", ResourceGeneration: 1, Capability: "terminal", Direction: direction, Port: 443, ExpiresAt: 1 << 40}}
	}
	grant := func(self, other string, direction string) Grant {
		g := Grant{AccountID: "account", WireGuardPublicKey: self}
		for i := range MaxMergedPeers - 1 {
			g.Peers = append(g.Peers, Peer{WireGuardPublicKey: fmt.Sprintf("peer-%04d", i), Scopes: scope(direction)})
		}
		g.Peers = append(g.Peers, Peer{WireGuardPublicKey: other, Scopes: scope(direction)})
		return g.withIndex()
	}
	a, c := grant("key-a", "key-c", "dial"), grant("key-c", "key-a", "accept")
	if !allowedAdmitted(a, c) {
		b.Fatal("pair not admitted")
	}
	b.ReportAllocs()
	for b.Loop() {
		if !allowedAdmitted(a, c) {
			b.Fatal("pair not admitted")
		}
	}
}
