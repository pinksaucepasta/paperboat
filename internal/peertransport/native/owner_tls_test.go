package native

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"math/big"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/peertransport/tailnet"
)

func TestBoundTLSAuthenticatesStableQUICKeyAcrossLeafRotation(t *testing.T) {
	now := time.Now().UTC()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	peer := tailnet.NetworkBinding{QUICPublicKey: base64.RawURLEncoding.EncodeToString(public)}
	verify := boundTLS(&tls.Config{}, peer, false).VerifyConnection
	first := issueTestLeaf(t, private, now.Add(-time.Minute), now.Add(time.Hour), 1)
	second := issueTestLeaf(t, private, now.Add(-time.Minute), now.Add(2*time.Hour), 2)
	for _, leaf := range []*x509.Certificate{first, second} {
		if err := verify(tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf}}); err != nil {
			t.Fatalf("valid rotated leaf rejected: %v", err)
		}
	}
	_, wrongPrivate, _ := ed25519.GenerateKey(rand.Reader)
	for name, leaf := range map[string]*x509.Certificate{
		"wrong key": issueTestLeaf(t, wrongPrivate, now.Add(-time.Minute), now.Add(time.Hour), 3),
		"expired":   issueTestLeaf(t, private, now.Add(-2*time.Hour), now.Add(-time.Hour), 4),
	} {
		if err := verify(tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf}}); err == nil {
			t.Fatalf("%s leaf accepted", name)
		}
	}
	corrupt := *first
	corrupt.Signature = append([]byte(nil), first.Signature...)
	corrupt.Signature[0] ^= 1
	if err := verify(tls.ConnectionState{PeerCertificates: []*x509.Certificate{&corrupt}}); err == nil {
		t.Fatal("invalid self-signature accepted")
	}
}

func issueTestLeaf(t *testing.T, private ed25519.PrivateKey, notBefore, notAfter time.Time, serial int64) *x509.Certificate {
	t.Helper()
	template := &x509.Certificate{SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: "peer"}, NotBefore: notBefore, NotAfter: notAfter, KeyUsage: x509.KeyUsageDigitalSignature}
	raw, err := x509.CreateCertificate(rand.Reader, template, template, private.Public(), private)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(raw)
	if err != nil {
		t.Fatal(err)
	}
	return leaf
}
