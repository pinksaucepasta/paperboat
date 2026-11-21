package main

import (
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/cli/internal/environmente2ee"
	wire "github.com/pinksaucepasta/paperboat/shared/recording"
)

// With no key yet, reset and replace say the first recording creates it (never "only the team owner"); a reset by
// anyone but the owner's vault is refused with what to do instead.
func TestRecordingKeyRequestFirstKeyAndRefusals(t *testing.T) {
	viewer, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	seed := make([]byte, ed25519.SeedSize)
	rand.Read(seed)
	keys := &environmente2ee.VaultKeys{WriterSeed: seed, SharingPrivate: viewer.Bytes()}
	none := wire.KeyState{Viewers: []wire.Viewer{{AccountID: "owner", SharingPublicKey: viewer.PublicKey().Bytes()}}}

	for _, reset := range []bool{true, false} {
		if _, err := recordingKeyRequest(reset, "team_1", none, keys); err == nil || !strings.Contains(err.Error(), "created when the first recording starts in the console") {
			t.Fatalf("reset=%v with no key: %v", reset, err)
		}
	}
	first, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signingPublic, _, _ := ed25519.GenerateKey(rand.Reader)
	ownerSeed := make([]byte, ed25519.SeedSize)
	rand.Read(ownerSeed)
	gen1 := wire.Key{Generation: 1, PublicKey: first.PublicKey().Bytes(), SigningPublicKey: signingPublic}
	gen1.ResetPublicKey = ed25519.NewKeyFromSeed(ownerSeed).Public().(ed25519.PublicKey)
	if _, err := recordingKeyRequest(true, "team_1", wire.KeyState{Key: &gen1, Viewers: none.Viewers}, keys); err == nil || !strings.Contains(err.Error(), "only the team owner") {
		t.Fatalf("non-owner reset: %v", err)
	}
	keys.WriterSeed = ownerSeed
	if reset, err := recordingKeyRequest(true, "team_1", wire.KeyState{Key: &gen1, Viewers: none.Viewers}, keys); err != nil || !wire.VerifyReset("team_1", gen1, reset.Key) {
		t.Fatalf("owner reset: %v", err)
	}
}
