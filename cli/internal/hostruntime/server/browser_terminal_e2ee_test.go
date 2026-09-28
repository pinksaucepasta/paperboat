package server

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"math/big"
	"testing"
	"time"
)

func TestBrowserTerminalWebSocketWriteDeadlineIsBoundedAndRespectsEarlierLimit(t *testing.T) {
	stream := newBrowserTerminalWebSocketStream(context.Background(), nil)
	defer stream.Close()

	defaultContext, cancelDefault := stream.operationContext(false)
	defaultDeadline, ok := defaultContext.Deadline()
	cancelDefault()
	if !ok {
		t.Fatal("WebSocket write has no deadline")
	}
	remaining := time.Until(defaultDeadline)
	if remaining <= 0 || remaining > browserTerminalWriteTimeout {
		t.Fatalf("default write deadline has invalid remaining duration %s", remaining)
	}

	earlier := time.Now().Add(100 * time.Millisecond)
	if err := stream.SetWriteDeadline(earlier); err != nil {
		t.Fatal(err)
	}
	boundedContext, cancelBounded := stream.operationContext(false)
	boundedDeadline, ok := boundedContext.Deadline()
	cancelBounded()
	if !ok || boundedDeadline.After(earlier) {
		t.Fatalf("earlier caller deadline was not preserved: deadline=%v earlier=%v", boundedDeadline, earlier)
	}
}

func TestBrowserTerminalClientCertificateMustMatchSignedKeyPin(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	certificate, digest := browserTerminalTestCertificate(t, now)
	if err := verifyBrowserTerminalClientCertificate([][]byte{certificate}, digest, now); err != nil {
		t.Fatalf("matching ephemeral client certificate rejected: %v", err)
	}
	otherCertificate, otherDigest := browserTerminalTestCertificate(t, now)
	if err := verifyBrowserTerminalClientCertificate([][]byte{certificate}, otherDigest, now); err == nil {
		t.Fatal("client certificate with a different signed SPKI pin was accepted")
	}
	if err := verifyBrowserTerminalClientCertificate([][]byte{otherCertificate}, digest, now); err == nil {
		t.Fatal("different browser private key was accepted for the ticket")
	}
}

func browserTerminalTestCertificate(t *testing.T, now time.Time) ([]byte, string) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "browser terminal test"},
		NotBefore:    now.Add(-time.Minute),
		NotAfter:     now.Add(4 * time.Minute),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	encoded, err := x509.CreateCertificate(rand.Reader, template, template, public, private)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(encoded)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(leaf.RawSubjectPublicKeyInfo)
	return encoded, base64.RawURLEncoding.EncodeToString(digest[:])
}
