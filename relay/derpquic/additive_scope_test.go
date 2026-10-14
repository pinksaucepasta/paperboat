package derpquic

import (
	"testing"
	"time"

	"tailscale.com/types/key"
)

func TestAdditiveResourceRequiresFreshReciprocalGrant(t *testing.T) {
	f := newFixture(t)
	s, address, _ := f.start("127.0.0.1:0")
	ka, kb := key.NewNode().Public(), key.NewNode().Public()
	ca, cb := certificate(t), certificate(t)
	ga, gb := f.grant(ka, ca, "a"), f.grant(kb, cb, "b")
	pairScopes(&ga, &gb)
	ga.Peers[0].Scopes[0].ResourceID = "new-access"
	gb.Peers[0].Scopes[0].ResourceID = "old-access"
	a, b := f.client(address, ca, ga), f.client(address, cb, gb)
	connect(t, a)
	connect(t, b)
	if err := a.SendControl(kb, []byte("new-resource")); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { return s.Snapshot().Dropped > 0 })
	if s.Snapshot().Forwarded != 0 {
		t.Fatal("old grant authorized newly added resource")
	}

	// Use the exact in-place authentication frame sent by Client.refresh. The
	// existing receiver connection must survive while gaining the new scope.
	b.mu.Lock()
	cc := b.current
	b.mu.Unlock()
	gb.Generation++
	newScope := gb.Peers[0].Scopes[0]
	newScope.ResourceID = "new-access"
	gb.Peers[0].Scopes = append(gb.Peers[0].Scopes, newScope)
	if err := cc.frame(frameAuth, []byte(f.token(gb))); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.peers[kb] != nil && s.peers[kb].grant.Generation == gb.Generation
	})
	if err := a.SendControl(kb, []byte("new-resource")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-cc.receive:
	case <-time.After(time.Second):
		t.Fatal("fresh reciprocal grant did not permit control delivery")
	}
	if err := s.Revoke("account", "b", gb.Generation+1); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.peers[kb] == nil
	})
	forwarded := s.Snapshot().Forwarded
	if err := a.SendControl(kb, []byte("revoked-resource")); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { return s.Snapshot().Dropped > 1 })
	if s.Snapshot().Forwarded != forwarded {
		t.Fatal("revoked receiver retained routing authority")
	}
}
