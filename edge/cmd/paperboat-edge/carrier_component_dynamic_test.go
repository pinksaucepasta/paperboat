package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/edge/internal/datacarrier"
	"github.com/pinksaucepasta/paperboat/shared/connectorprotocol"
)

// h2OnlyTransport speaks HTTP/2 only, so a carrier CONNECT is never downgraded to HTTP/1.
func h2OnlyTransport(config *tls.Config) *http.Transport {
	protocols := new(http.Protocols)
	protocols.SetHTTP2(true)
	return &http.Transport{TLSClientConfig: config, Protocols: protocols}
}

func dynamicCarrierCertificate(t *testing.T, commonName string, public ed25519.PublicKey, private ed25519.PrivateKey) tls.Certificate {
	t.Helper()
	if public == nil || private == nil {
		var err error
		public, private, err = ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
	}
	return dynamicCarrierCertificateWithTemplate(t, commonName, public, private, nil)
}

func dynamicCarrierCertificateWithURI(t *testing.T, commonName string, public ed25519.PublicKey, private ed25519.PrivateKey, identity datacarrier.Identity, edgeProcess string) (tls.Certificate, error) {
	uri, err := connectorprotocol.CarrierIdentityURN(connectorprotocol.CarrierIdentityBinding{
		AccountID: identity.AccountID, HostID: identity.HostID, TunnelID: identity.TunnelID,
		ConnectorID: identity.ConnectorID, SessionID: identity.SessionID,
		ProcessGeneration: identity.ProcessGeneration, ConfigGeneration: identity.Generation,
		EdgeProcessEpoch: edgeProcess,
	})
	if err != nil {
		return tls.Certificate{}, err
	}
	return dynamicCarrierCertificateWithTemplate(t, commonName, public, private, uri), nil
}

func dynamicCarrierCertificateWithTemplate(t *testing.T, commonName string, public ed25519.PublicKey, private ed25519.PrivateKey, uri *url.URL) tls.Certificate {
	t.Helper()
	now := time.Now().UTC()
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: commonName},
		NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
	}
	if uri != nil {
		template.URIs = []*url.URL{uri}
		template.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, public, private)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: private}
}

func dynamicCarrierListenAddresses(t *testing.T) (string, string) {
	t.Helper()
	tcp, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	tcpAddress := tcp.Addr().String()
	_ = tcp.Close()
	quic, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	quicAddress := quic.LocalAddr().String()
	_ = quic.Close()
	return tcpAddress, quicAddress
}

func waitDynamicCondition(t *testing.T, ctx context.Context, condition func() bool) {
	t.Helper()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		if condition() {
			return
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatalf("condition did not become true: %v", ctx.Err())
		}
	}
}

type dynamicHTTPLink struct {
	io.ReadCloser
	writer *io.PipeWriter
}

func (l dynamicHTTPLink) Write(p []byte) (int, error) { return l.writer.Write(p) }
func (l dynamicHTTPLink) Close() error                { _ = l.writer.Close(); return l.ReadCloser.Close() }

func TestCarrierHandlerLimitFollowsNodeCapacity(t *testing.T) {
	for capacity, want := range map[uint32]int{0: 64, 1: 1, 1024: 1024, 1025: 1025, 4096: 4096} {
		if got := carrierHandlerLimit(capacity); got != want {
			t.Fatalf("carrierHandlerLimit(%d) = %d, want %d", capacity, got, want)
		}
	}
}
