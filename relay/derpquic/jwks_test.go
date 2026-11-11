package derpquic

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/shared/jwks"
	"tailscale.com/types/key"
)

func jwksDocument(t *testing.T, withdrawn []string, keys map[string]ed25519.PublicKey) []byte {
	t.Helper()
	type jwk struct{ Kty, Crv, Use, Alg, Kid, X string }
	document := struct {
		Keys      []jwk    `json:"keys"`
		Withdrawn []string `json:"withdrawn_key_ids,omitempty"`
	}{Withdrawn: withdrawn}
	for id, k := range keys {
		document.Keys = append(document.Keys, jwk{"OKP", "Ed25519", "sig", "EdDSA", id, base64.RawURLEncoding.EncodeToString(k)})
	}
	raw, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// tokenFor signs g with priv under kid, like fixture.token does for its own key.
func (f *fixture) tokenFor(g Grant, kid string, priv ed25519.PrivateKey) string {
	h := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"alg":"EdDSA","typ":"paperboat-relay-grant+jwt","kid":%q}`, kid)))
	b, _ := json.Marshal(g)
	body := h + "." + base64.RawURLEncoding.EncodeToString(b)
	return body + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(priv, []byte(body)))
}

func TestServerKeysRotateOverlapAndWithdraw(t *testing.T) {
	f := newFixture(t)
	oldPub := f.verifier.Keys["test"]
	newPub, newPriv, _ := ed25519.GenerateKey(rand.Reader)
	server, err := NewServer(f.verifier)
	if err != nil {
		t.Fatal(err)
	}
	cert := f.tls.Certificates[0]
	state := tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert.Leaf}}
	g := f.grant(key.NewNode().Public(), cert, "rotating")
	oldToken, newToken := f.token(g), f.tokenFor(g, "new", newPriv)
	if _, err = server.verify(newToken, state, time.Now()); !errors.Is(err, ErrAdmission) {
		t.Fatalf("a key the relay was never given verified: %v", err)
	}
	// Overlap: the control plane lists the new key before it withdraws the old one.
	keys, err := jwks.Parse(jwksDocument(t, nil, map[string]ed25519.PublicKey{"test": oldPub, "new": newPub}))
	if err != nil || server.SetKeys(keys) != nil {
		t.Fatal("overlap keys refused", err)
	}
	for name, token := range map[string]string{"old": oldToken, "new": newToken} {
		if _, err = server.verify(token, state, time.Now()); err != nil {
			t.Fatalf("%s key refused during the overlap: %v", name, err)
		}
	}
	keys, err = jwks.Parse(jwksDocument(t, []string{"test"}, map[string]ed25519.PublicKey{"test": oldPub, "new": newPub}))
	if err != nil || server.SetKeys(keys) != nil || !slices.Equal(server.KeyIDs(), []string{"new"}) {
		t.Fatalf("withdrawal not applied: %v %v", server.KeyIDs(), err)
	}
	if _, err = server.verify(oldToken, state, time.Now()); err == nil {
		t.Fatal("a withdrawn key still verifies")
	}
	if _, err = server.verify(newToken, state, time.Now()); err != nil {
		t.Fatalf("new key refused after the withdrawal: %v", err)
	}
	if server.SetKeys(nil) == nil {
		t.Fatal("an empty key set was accepted")
	}
}

func TestUnknownKeyRefreshesOnceAndIsRateLimited(t *testing.T) {
	f := newFixture(t)
	newPub, newPriv, _ := ed25519.GenerateKey(rand.Reader)
	server, err := NewServer(f.verifier)
	if err != nil {
		t.Fatal(err)
	}
	cert := f.tls.Certificates[0]
	state := tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert.Leaf}}
	g := f.grant(key.NewNode().Public(), cert, "unknown_kid")
	calls := 0
	server.SetKeyRefresher(func() error {
		calls++
		return server.SetKeys(map[string]ed25519.PublicKey{"test": f.verifier.Keys["test"], "new": newPub})
	})
	if _, err = f.verifier.Verify(f.token(g), state, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err = server.verify(f.token(g), state, time.Now()); err != nil || calls != 0 {
		t.Fatalf("a known key refreshed (%d calls) or failed: %v", calls, err)
	}
	if _, err = server.verify(f.tokenFor(g, "new", newPriv), state, time.Now()); err != nil || calls != 1 {
		t.Fatalf("unknown key not refreshed and retried: %d calls, %v", calls, err)
	}
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	for range 3 {
		if _, err = server.verify(f.tokenFor(g, "forged", other), state, time.Now()); !errors.Is(err, ErrExpired) {
			t.Fatalf("rate-limited unknown key should require fresh authority: %v", err)
		}
	}
	if calls != 1 {
		t.Fatalf("refresh ran %d times within a minute, want 1", calls)
	}
	server.keysRefreshedAt.Store(time.Now().Add(-2 * keyRefreshInterval).UnixNano())
	_, _ = server.verify(f.tokenFor(g, "forged", other), state, time.Now())
	if calls != 2 {
		t.Fatalf("refresh did not run again after the interval: %d calls", calls)
	}
}

func TestSigningKeyRefreshFailureDoesNotLatchAdmission(t *testing.T) {
	f := newFixture(t)
	pub, private, _ := ed25519.GenerateKey(rand.Reader)
	server, err := NewServer(f.verifier)
	if err != nil {
		t.Fatal(err)
	}
	cert := f.tls.Certificates[0]
	state := tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert.Leaf}}
	token := f.tokenFor(f.grant(key.NewNode().Public(), cert, "rotating"), "new", private)
	calls := 0
	server.SetKeyRefresher(func() error { calls++; return errors.New("offline") })
	for range 2 {
		if _, err := server.verify(token, state, time.Now()); !errors.Is(err, ErrExpired) {
			t.Fatalf("refresh outage became permanent: %v", err)
		}
	}
	if calls != 1 {
		t.Fatalf("refresh calls = %d, want one bounded attempt", calls)
	}
	server.keysRefreshedAt.Store(0)
	server.SetKeyRefresher(func() error { return server.SetKeys(map[string]ed25519.PublicKey{"new": pub}) })
	if _, err := server.verify(token, state, time.Now()); err != nil {
		t.Fatalf("valid new key did not recover: %v", err)
	}
	server.keysRefreshedAt.Store(0)
	if _, err := server.verify(f.tokenFor(f.grant(key.NewNode().Public(), cert, "unknown"), "forged", private), state, time.Now()); !errors.Is(err, ErrAdmission) {
		t.Fatalf("refreshed key set did not permanently refuse unknown key: %v", err)
	}
}
