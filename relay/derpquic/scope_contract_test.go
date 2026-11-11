package derpquic

import (
	"crypto/tls"
	"crypto/x509"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/shared/peernetwork"
	"tailscale.com/types/key"
)

// The relay admits a signed grant carrying any scope the shared list has, per peer and as a membership. A recording
// scope the relay did not know refused the whole grant, so every relayed pair of the account died (2026-11-11).
func TestSignedGrantAdmitsEverySharedScope(t *testing.T) {
	f := newFixture(t)
	verify := func(t *testing.T, g Grant, cert tls.Certificate) {
		t.Helper()
		if _, err := f.verifier.Verify(f.token(g), tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert.Leaf}}, time.Now()); err != nil {
			t.Fatalf("grant refused: %v", err)
		}
	}
	shared := 0
	peernetwork.Scopes(func(kind, capability string) bool {
		shared++
		t.Run(kind+"/"+capability, func(t *testing.T) {
			cert := certificate(t)
			g := f.grant(key.NewNode().Public(), cert, "endpoint")
			s := Scope{ResourceKind: kind, ResourceID: "rec_01234567-89ab-cdef-0123-456789abcdef", ResourceGeneration: 1, Capability: capability, Direction: "dial", Port: 443, ExpiresAt: g.ExpiresAt}
			g.Peers = []Peer{{WireGuardPublicKey: KeyString(key.NewNode().Public()), Scopes: []Scope{s}}}
			verify(t, g, cert)
			g.Peers, g.Memberships = nil, []Scope{s}
			verify(t, g, cert)
		})
		return true
	})
	if shared == 0 {
		t.Fatal("no shared scopes")
	}
	// A CLI replay's recording scope beside the account mesh pair, as the server mints it for a viewer's endpoint.
	cert := certificate(t)
	g := f.grant(key.NewNode().Public(), cert, "viewer_cli")
	g.Peers = []Peer{{WireGuardPublicKey: KeyString(key.NewNode().Public()), Scopes: []Scope{
		{ResourceKind: "machine_network", ResourceID: "network_account", ResourceGeneration: 1, Capability: "connect", Direction: "dial", Port: 443, ExpiresAt: g.ExpiresAt},
		{ResourceKind: "recording", ResourceID: "rec_01234567-89ab-cdef-0123-456789abcdef", ResourceGeneration: 1, Capability: "recording", Direction: "dial", Port: 443, ExpiresAt: g.ExpiresAt},
	}}}
	verify(t, g, cert)
}
