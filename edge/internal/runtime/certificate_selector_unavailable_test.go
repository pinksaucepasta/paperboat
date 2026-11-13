package runtime

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"github.com/pinksaucepasta/paperboat/edge/internal/control"
	"math/big"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/edge/internal/tlscert"
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

// 2026-11-05: the not-found page used the full selector, so every handshake to a stopped or unknown name
// asked the server for an on-demand certificate (5xx flood at /v1/edge/certificates/distributions/request).
// The wildcard-only config completes TLS for covered names without one on-demand request.
func TestWildcardTLSConfigNeverRequestsOnDemandCertificates(t *testing.T) {
	cert, pool := wildcardTestCertificate(t, "*.preview.example.test")
	requests := &countingOnDemand{}
	selector, err := NewCertificateSelector(CertificateSelectorConfig{Source: missingSource{}, Wildcard: wildcardOnly{cert}, OnDemandRequester: requests, HandshakeTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if err := handshake(t, selector.WildcardTLSConfig(), pool, "stopped.preview.example.test"); err != nil {
		t.Fatalf("handshake for a stopped name: %v", err)
	}
	if err := handshake(t, selector.WildcardTLSConfig(), pool, "other.example.test"); err == nil {
		t.Fatal("a name outside the wildcard completed TLS")
	}
	if n := requests.count.Load(); n != 0 {
		t.Fatalf("on-demand certificate requests = %d, want 0", n)
	}
}

type countingOnDemand struct{ count atomic.Int32 }

func (c *countingOnDemand) RequestCertificate(context.Context, string) error {
	c.count.Add(1)
	return errors.New("refused")
}

// An on-demand request the server answers "not issued yet" (409) still completes TLS on the platform
// wildcard, as any failed on-demand request does, and the next handshake asks again.
func TestCertificateSelectorFallsBackWhenCertificateNotIssuedYet(t *testing.T) {
	cert, pool := wildcardTestCertificate(t, "*.preview.example.test")
	requests := &notIssuedYetOnDemand{}
	selector, err := NewCertificateSelector(CertificateSelectorConfig{Source: missingSource{}, Wildcard: wildcardOnly{cert}, OnDemandRequester: requests, HandshakeTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	for attempt := 1; attempt <= 2; attempt++ {
		if err := handshake(t, selector.TLSConfig(), pool, "pending.preview.example.test"); err != nil {
			t.Fatalf("handshake %d while the certificate is not issued yet: %v", attempt, err)
		}
	}
	if n := requests.count.Load(); n != 2 {
		t.Fatalf("on-demand certificate requests = %d, want 2 (one per handshake)", n)
	}
}

type notIssuedYetOnDemand struct{ count atomic.Int32 }

func (c *notIssuedYetOnDemand) RequestCertificate(context.Context, string) error {
	c.count.Add(1)
	return control.ErrCertificateNotIssuedYet
}
