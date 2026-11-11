package derpquic

import (
	"crypto/ed25519"
	"crypto/rand"
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

// Grants live 12 h, so fences outlive their connections and the table can fill with idle endpoints; it then drops
// an idle admission fence for a new endpoint, never a connected one's and never a revocation's.
func TestFullFenceTableEvictsOnlyIdleAdmissionFences(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	s, err := NewServer(Verifier{Issuer: "test", NodeID: "relay", NodeGeneration: 1, ProcessEpoch: "epoch", Keys: map[string]ed25519.PublicKey{"test": pub}})
	if err != nil {
		t.Fatal(err)
	}
	soon := time.Now().Add(time.Hour).Unix()
	s.mu.Lock()
	for i := range MaxConnections {
		id := fmt.Sprintf("account\x00endpoint-%d", i)
		s.fences[id] = &grantFence{Grant{RelayGrant: peernetwork.RelayGrant{AccountID: "account", EndpointID: fmt.Sprintf("endpoint-%d", i), Generation: 1, ExpiresAt: soon + int64(i)}}}
	}
	revoked := "account\x00endpoint-0"
	s.fences[revoked] = &grantFence{Grant{RelayGrant: peernetwork.RelayGrant{Generation: 5, ExpiresAt: soon}}} // a revocation: no signed grant
	if !s.evictIdleFenceLocked() {
		t.Fatal("a full table with idle admission fences made no room")
	}
	if s.fences[revoked] == nil || len(s.fences) != MaxConnections-1 {
		t.Fatalf("evicted the revocation fence or the wrong count: %d", len(s.fences))
	}
	if s.fences["account\x00endpoint-1"] != nil {
		t.Fatal("did not drop the admission fence that ends soonest")
	}
	for id, f := range s.fences {
		s.fences[id] = &grantFence{Grant{RelayGrant: peernetwork.RelayGrant{Generation: 2, ExpiresAt: f.ExpiresAt}}}
	}
	if s.evictIdleFenceLocked() {
		t.Fatal("dropped a revocation fence")
	}
	s.mu.Unlock()
}
