package derpquic

import (
	"crypto/tls"
	"crypto/x509"
	"tailscale.com/types/key"
	"testing"
	"time"
)

func TestInspectorOnlyRelayAuthority(t *testing.T) {
	f := newFixture(t)
	cert := f.tls.Certificates[0]
	a := f.grant(key.NewNode().Public(), cert, "inspector_cli")
	b := f.grant(key.NewNode().Public(), cert, "owner_machine")
	a.AccountID = "teammate"
	pairScopes(&a, &b)
	for _, g := range []*Grant{&a, &b} {
		g.Memberships[0].ResourceKind = "inspector"
		g.Memberships[0].ResourceID = "inspector_credential"
		g.Memberships[0].Capability = "inspector"
	}
	state := tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert.Leaf}}
	if _, err := f.verifier.Verify(f.token(a), state, time.Now()); err != nil {
		t.Fatal(err)
	}
	if !allowed(a, b, time.Now()) {
		t.Fatal("cross-account inspector grant denied")
	}
	// (inspector, terminal) is no scope of the shared list: it authorizes nothing (dropped like a newer
	// server's scope) and never fails the rest of the grant.
	a.Memberships[0].Capability = "terminal"
	if got, err := f.verifier.Verify(f.token(a), state, time.Now()); err != nil || len(got.Memberships) != 0 {
		t.Fatalf("inspector resource kept a terminal capability: %+v, %v", got.Memberships, err)
	}
	if allowed(a, b, time.Now()) {
		t.Fatal("different capability matched inspector peer")
	}
}
