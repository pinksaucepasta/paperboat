package derpquic

import (
	"errors"
	"fmt"
	"github.com/pinksaucepasta/paperboat/shared/peernetwork"
	"testing"
	"time"

	"tailscale.com/types/key"
)

// A grant lives MaxGrantLifetime (12 h) so a relayed path can be opened while Paperboat is unreachable, and an
// explicit revocation still ends it at once: the carrier closes and the same grant is refused by the fence.
func TestLongGrantAdmittedAndRevokedByFence(t *testing.T) {
	f := newFixture(t)
	s, address, _ := f.start("127.0.0.1:0")
	ka, kb := key.NewNode().Public(), key.NewNode().Public()
	ca, cb := certificate(t), certificate(t)
	ga, gb := f.grant(ka, ca, "a"), f.grant(kb, cb, "b")
	ga.ExpiresAt, gb.ExpiresAt = ga.IssuedAt+int64(MaxGrantLifetime/time.Second), gb.IssuedAt+int64(MaxGrantLifetime/time.Second)
	pairScopes(&ga, &gb)
	a, b := f.client(address, ca, ga), f.client(address, cb, gb)
	if err := a.Connect(t.Context()); err != nil {
		t.Fatal("12 h grant refused: ", err)
	}
	connect(t, b)
	if err := a.Send(kb, []byte("hello")); err != nil {
		t.Fatal(err)
	}
	receive(t, b, ka, []byte("hello"))

	tooLong := f.grant(key.NewNode().Public(), certificate(t), "long")
	tooLong.ExpiresAt = tooLong.IssuedAt + int64(MaxGrantLifetime/time.Second) + 1
	if err := f.client(address, certificate(t), tooLong).Connect(t.Context()); !errors.Is(err, ErrAdmission) {
		t.Fatalf("a grant past MaxGrantLifetime: %v, want ErrAdmission", err)
	}

	closed := make(chan error, 1)
	go func() { _, _, err := a.RecvDetail(); closed <- err }()
	if err := s.Revoke("account", "a", ga.Generation+1); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-closed:
		if err == nil {
			t.Fatal("revocation left the carrier readable")
		}
	case <-time.After(time.Second):
		t.Fatal("revocation did not close the carrier")
	}
	if err := f.client(address, ca, ga).Connect(t.Context()); !errors.Is(err, ErrAdmission) {
		t.Fatalf("revoked endpoint re-admitted with its unexpired 12 h grant: %v", err)
	}
}

// Capacity must never erase a live admission high-water mark: an older signed
// grant could otherwise be replayed for the next twelve hours.
func TestFullFenceTableRetainsAdmissionHighWaterMarks(t *testing.T) {
	f := newFixture(t)
	s, err := NewServer(f.verifier)
	if err != nil {
		t.Fatal(err)
	}
	soon := time.Now().Add(time.Hour).Unix()
	for i := range MaxConnections {
		endpoint := fmt.Sprintf("endpoint-%d", i)
		s.fences["account\x00"+endpoint] = newGrantFence(Grant{RelayGrant: peernetwork.RelayGrant{AccountID: "account", EndpointID: endpoint, Generation: 5, ExpiresAt: soon + int64(i)}}, time.Now())
	}
	cert := certificate(t)
	g := f.grant(key.NewNode().Public(), cert, "new_endpoint")
	if err := s.admit(&peerConn{}, g, time.Now()); !errors.Is(err, ErrOverload) {
		t.Fatalf("full fence table accepted new endpoint: %v", err)
	}
	if len(s.fences) != MaxConnections || s.fences["account\x00endpoint-0"] == nil {
		t.Fatal("capacity evicted an admission high-water mark")
	}
	g.EndpointID, g.Generation = "endpoint-0", 1
	if err := s.admit(&peerConn{}, g, time.Now()); !errors.Is(err, ErrAdmission) {
		t.Fatalf("older grant was readmitted after capacity pressure: %v", err)
	}
	if err := s.Revoke("account", "new_revocation", 2); !errors.Is(err, ErrOverload) {
		t.Fatalf("unbounded revocation admission: %v", err)
	}
	if err := s.Revoke("account", "endpoint-0", 6); err != nil {
		t.Fatalf("existing endpoint could not be revoked at capacity: %v", err)
	}
	if s.fences["account\x00endpoint-1"] == nil {
		t.Fatal("revocation erased an unrelated fence")
	}
}

func TestShorterGrantCannotExpireItsReplayFence(t *testing.T) {
	f := newFixture(t)
	s, err := NewServer(f.verifier)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	g := f.grant(key.NewNode().Public(), certificate(t), "endpoint")
	g.Generation, g.ExpiresAt = 3, now.Add(time.Second).Unix()
	if err := s.admit(&peerConn{}, g, now); err != nil {
		t.Fatal(err)
	}
	if s.fences[grantID(g)].ExpiresAt != g.ExpiresAt {
		t.Fatal("retention extended authorization expiry")
	}
	s.peers = make(map[key.NodePublic]*peerConn) // endpoint disconnected
	old := g
	old.Generation, old.ExpiresAt = 2, now.Add(time.Hour).Unix()
	if err := s.admit(&peerConn{}, old, now.Add(2*time.Second)); !errors.Is(err, ErrAdmission) {
		t.Fatalf("older long-lived grant replayed after newer grant expiry: %v", err)
	}
}
