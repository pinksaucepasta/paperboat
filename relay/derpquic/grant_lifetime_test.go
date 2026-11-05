package derpquic

import (
	"testing"
	"time"

	"tailscale.com/types/key"
)

// Grants live MaxGrantLifetime (10 min) so an idle client renews at about half life instead of every minute. A
// longer-lived grant must not delay revocation: revoking an endpoint closes its held connection while its grant is
// still valid, and its fence refuses that grant for as long as the grant could be presented.
func TestRevokeClosesHeldRelayConnectionWhileItsGrantIsValid(t *testing.T) {
	f := newFixture(t)
	s, address, _ := f.start("127.0.0.1:0")
	ka, kb := key.NewNode().Public(), key.NewNode().Public()
	ca, cb := certificate(t), certificate(t)
	ga, gb := f.grant(ka, ca, "a"), f.grant(kb, cb, "b")
	ga.ExpiresAt = ga.IssuedAt + int64(MaxGrantLifetime/time.Second)
	gb.ExpiresAt = ga.ExpiresAt
	pairScopes(&ga, &gb)
	tooLong := ga
	tooLong.ExpiresAt++
	if err := f.client(address, ca, tooLong).Connect(t.Context()); err == nil {
		t.Fatal("a grant longer than MaxGrantLifetime was admitted")
	}
	a, b := f.client(address, ca, ga), f.client(address, cb, gb)
	connect(t, a)
	connect(t, b)
	if err := a.Send(kb, []byte("before")); err != nil {
		t.Fatal(err)
	}
	receive(t, b, ka, []byte("before"))

	closed := make(chan error, 1)
	go func() { _, _, err := a.RecvDetail(); closed <- err }()
	revoked := time.Now()
	if err := s.Revoke("account", "a", ga.Generation+1); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-closed:
		if err == nil {
			t.Fatal("revocation left the held connection readable")
		}
	case <-time.After(time.Second):
		t.Fatal("revocation did not close a held connection whose grant is valid for 10 more minutes")
	}
	if err := f.client(address, ca, ga).Connect(t.Context()); err == nil {
		t.Fatal("the revoked, still valid grant was admitted again")
	}
	s.mu.Lock()
	fence := s.fences["account\x00a"]
	s.mu.Unlock()
	if fence == nil || fence.ExpiresAt < revoked.Add(MaxGrantLifetime).Unix() {
		t.Fatalf("revocation fence %+v ends before the revoked grant could no longer be presented (%d)", fence, ga.ExpiresAt)
	}
}
