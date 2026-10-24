package auth

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"testing"
)

func withdrawn(t *testing.T, document []byte, ids ...string) []byte {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(document, &doc); err != nil {
		t.Fatal(err)
	}
	doc["withdrawn_key_ids"] = ids
	out, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// A key the server withdrew (F21-08 withdrawn_key_ids) is refused at once even while it is still listed; a
// document that would leave no key is refused and the last good set stays.
func TestWithdrawnKeysAreRefusedWhileStillListed(t *testing.T) {
	oldPublic, _, _ := ed25519.GenerateKey(rand.Reader)
	newPublic, _, _ := ed25519.GenerateKey(rand.Reader)
	keys := map[string]ed25519.PublicKey{"old": oldPublic, "new": newPublic}
	snapshot := NewSnapshot()
	if err := snapshot.ReplaceJWKS(withdrawn(t, makeJWKS(t, keys), "old")); err != nil {
		t.Fatal(err)
	}
	if _, err := snapshot.Key(context.Background(), "old"); err == nil {
		t.Fatal("withdrawn key still verifies")
	}
	if _, err := snapshot.Key(context.Background(), "new"); err != nil {
		t.Fatalf("active key refused: %v", err)
	}
	if err := snapshot.ReplaceJWKS(withdrawn(t, makeJWKS(t, keys), "old", "new")); err == nil {
		t.Fatal("a document withdrawing every key was accepted")
	}
	if _, err := snapshot.Key(context.Background(), "new"); err != nil {
		t.Fatal("a refused document replaced the last good keys")
	}
	if err := snapshot.ReplaceJWKS(withdrawn(t, makeJWKS(t, keys), "")); err == nil {
		t.Fatal("an empty withdrawn key id was accepted")
	}
}
