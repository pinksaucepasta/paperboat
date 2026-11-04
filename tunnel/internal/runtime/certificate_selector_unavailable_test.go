package runtime

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/tunnel/internal/tlscert"
)

type missingSource struct{}

func (missingSource) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	return nil, tlscert.ErrCertificateMissing
}

type wildcardOnly struct{ cert *tls.Certificate }

func (w wildcardOnly) WildcardCertificate(string) (*tls.Certificate, error) { return w.cert, nil }

func wildcardTestCertificate(t *testing.T, name string) (*tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: name}, DNSNames: []string{name}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	return &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, pool
}

func handshake(t *testing.T, config *tls.Config, pool *x509.CertPool, host string) error {
	t.Helper()
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	deadline := time.Now().Add(5 * time.Second)
	client.SetDeadline(deadline)
	server.SetDeadline(deadline)
	go func() { _ = tls.Server(server, config).Handshake() }()
	return tls.Client(client, &tls.Config{ServerName: host, RootCAs: pool, MinVersion: tls.VersionTLS13}).Handshake()
}

// A route that is known but unavailable must still complete TLS (it was SSL_ERROR_SYSCALL).
func TestCertificateSelectorCompletesTLSForUnavailableRoute(t *testing.T) {
	cert, pool := wildcardTestCertificate(t, "*.preview.example.test")
	selector, err := NewCertificateSelector(CertificateSelectorConfig{Source: missingSource{}, Wildcard: wildcardOnly{cert}, HandshakeTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if err := handshake(t, selector.TLSConfig(), pool, "down.preview.example.test"); err != nil {
		t.Fatalf("handshake for unavailable route: %v", err)
	}
	// A stopped or unknown preview also completes TLS and gets the branded page (2026-11-04: a stopped
	// preview dropped the handshake with SSL_ERROR_SYSCALL); a name the wildcard does not cover is refused.
	if err := handshake(t, selector.TLSConfig(), pool, "stopped.preview.example.test"); err != nil {
		t.Fatalf("handshake for a stopped preview: %v", err)
	}
	if err := handshake(t, selector.TLSConfig(), pool, "other.example.test"); err == nil {
		t.Fatal("a name outside the wildcard completed TLS")
	}
}
