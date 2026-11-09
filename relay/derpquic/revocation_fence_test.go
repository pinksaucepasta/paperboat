package derpquic

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"
	"time"
)

// A revoked grant can stay valid here until MaxGrantLifetime after issue on a clock MaxClockSkew
// behind the issuer's; the revocation fence must hold at least that long or the grant replays.
func TestRevocationFenceOutlivesEveryRevokedGrant(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	s, err := NewServer(Verifier{Issuer: "test", NodeID: "relay", NodeGeneration: 1, ProcessEpoch: "epoch", Keys: map[string]ed25519.PublicKey{"test": pub}})
	if err != nil {
		t.Fatal(err)
	}
	before := time.Now()
	if err := s.Revoke("account", "endpoint", 3); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	fence := s.fences["account\x00endpoint"]
	s.mu.Unlock()
	if fence == nil || time.Unix(fence.ExpiresAt, 0).Before(before.Add(MaxGrantLifetime+MaxClockSkew).Truncate(time.Second)) {
		t.Fatalf("fence %+v ends before a revoked grant can (%s)", fence, MaxGrantLifetime+MaxClockSkew)
	}
}
