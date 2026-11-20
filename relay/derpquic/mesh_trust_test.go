package derpquic

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/shared/nodemesh"
)

// shortCertificate is certificate(t) valid for d only.
func shortCertificate(t *testing.T, d time.Duration) tls.Certificate {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	tmpl := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: "localhost"}, DNSNames: []string{"localhost"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(d), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}}
	raw, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(raw)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{raw}, PrivateKey: priv, Leaf: leaf}
}

// TestMeshPeerRemovedClosesLinkAndWithdraws: when the directory drops a peer its link ends and what it held is
// withdrawn at once, including the grants it relayed.
func TestMeshPeerRemovedClosesLinkAndWithdraws(t *testing.T) {
	a, b := meshPair(t, true)
	ka, kb, ca, cb := pairedClients(t, a, b)
	eventually(t, func() bool { return a.mesh.holder(kb) != nil && b.mesh.holder(ka) != nil })
	deliverEventually(t, ca, kb, cb, ka, payloadOf(200)) // b now caches a's grant, relayed by the link
	if err := b.mesh.SetPeers(nil); err != nil {
		t.Fatal(err)
	}
	b.mesh.mu.RLock()
	held, cached, links := len(b.mesh.holders), b.mesh.remote.len(), len(b.mesh.links)
	b.mesh.mu.RUnlock()
	if held != 0 || cached != 0 || links != 0 {
		t.Fatalf("after SetPeers(nil) holders %d, cached grants %d, links %d, want none", held, cached, links)
	}
	eventually(t, func() bool { return a.mesh.stats().Links == 0 && a.mesh.holderCount() == 0 })
}

// TestMeshRegisterRejectsDroppedPeer: a link whose handshake finished before the directory dropped or re-pinned its
// peer is not installed.
func TestMeshRegisterRejectsDroppedPeer(t *testing.T) {
	a, b := meshPair(t, false)
	link := func(p nodemesh.Peer) *meshLink {
		return &meshLink{m: a.mesh, peer: p, ctrl: make(chan meshFrame, meshControlDepth), out: make(chan meshOut, 1)}
	}
	if a.mesh.register(link(b.peer())) {
		t.Fatal("a peer absent from the directory was registered")
	}
	if err := a.mesh.SetPeers([]nodemesh.Peer{b.peer()}); err != nil {
		t.Fatal(err)
	}
	repinned := b.peer()
	repinned.SPKISHA256 = nodemesh.SPKIPin(certificate(t).Leaf)
	if a.mesh.register(link(repinned)) {
		t.Fatal("a peer under another pin was registered")
	}
	a.mesh.mu.RLock()
	defer a.mesh.mu.RUnlock()
	if len(a.mesh.links) != 0 {
		t.Fatal("a rejected link was listed")
	}
}

// TestMeshLinkEndsAtCertExpiry: a link is cut when the peer's certificate expires, and the redial is refused.
func TestMeshLinkEndsAtCertExpiry(t *testing.T) {
	fa, fb := relayFixtures(t)
	short := shortCertificate(t, 3*time.Second)
	fb.tls = &tls.Config{Certificates: []tls.Certificate{short}}
	a, b := startMeshNode(t, fa, "127.0.0.1:0"), startMeshNode(t, fb, "127.0.0.1:0")
	linkNodes(t, a, b)
	begun := time.Now()
	eventually(t, func() bool { return a.mesh.stats().Links == 0 })
	if time.Since(begun) > 6*time.Second {
		t.Fatalf("the link outlived the certificate by %v", time.Since(begun)-3*time.Second)
	}
	time.Sleep(500 * time.Millisecond) // redials hit the expired certificate at the handshake
	if a.mesh.stats().Links != 0 || b.mesh.stats().Links != 0 {
		t.Fatal("a link formed with an expired certificate")
	}
}
