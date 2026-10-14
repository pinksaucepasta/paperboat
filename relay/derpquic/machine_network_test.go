package derpquic

import (
	"crypto/tls"
	"crypto/x509"
	"testing"
	"time"

	"tailscale.com/types/key"
)

func TestPersonalNetworkAdmissionIsReciprocalAndAccountBound(t *testing.T) {
	f := newFixture(t)
	cert := f.tls.Certificates[0]
	a := f.grant(key.NewNode().Public(), cert, "personal_a")
	b := f.grant(key.NewNode().Public(), cert, "personal_b")
	pairScopes(&a, &b)
	for _, grant := range []*Grant{&a, &b} {
		grant.Peers[0].Scopes[0].ResourceKind = "machine_network"
		grant.Peers[0].Scopes[0].ResourceID = "network_pair"
		grant.Peers[0].Scopes[0].Capability = "connect"
	}
	state := tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert.Leaf}}
	if _, err := f.verifier.Verify(f.token(a), state, time.Now()); err != nil {
		t.Fatal(err)
	}
	if !allowed(a, b, time.Now()) {
		t.Fatal("reciprocal personal admission denied")
	}
	b.AccountID = "another_account"
	if allowed(a, b, time.Now()) {
		t.Fatal("personal network admission crossed accounts")
	}
	b.AccountID = a.AccountID
	b.Peers[0].Scopes[0].ResourceID = "unrelated_pair"
	if allowed(a, b, time.Now()) {
		t.Fatal("unrelated pair admission accepted")
	}
}

func TestRelayIssuedAtToleratesBoundedClockSkewWithoutExtendingExpiry(t *testing.T) {
	f := newFixture(t)
	cert := f.tls.Certificates[0]
	g := f.grant(key.NewNode().Public(), cert, "skew_client")
	state := tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert.Leaf}}
	now := time.Now()
	g.IssuedAt = now.Unix() + 3
	g.ExpiresAt = g.IssuedAt + 60
	if _, err := f.verifier.Verify(f.token(g), state, now); err != nil {
		t.Fatal("small clock skew rejected", err)
	}
	g.IssuedAt = now.Unix() + 61
	g.ExpiresAt = g.IssuedAt + 60
	if _, err := f.verifier.Verify(f.token(g), state, now); err == nil {
		t.Fatal("excessive clock skew accepted")
	}
	g.IssuedAt = now.Unix() - 60
	g.ExpiresAt = now.Unix()
	if _, err := f.verifier.Verify(f.token(g), state, now); err != ErrExpired {
		t.Fatal("expiry extended by clock tolerance", err)
	}
}
