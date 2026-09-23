//go:build linux || darwin || windows

package deviceguard

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"net"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/splitdns"
)

type certificateConnection struct{ identity string }

func (c *certificateConnection) Identity() (string, error)         { return c.identity, nil }
func (c *certificateConnection) Receive() (request, error)         { return request{}, net.ErrClosed }
func (c *certificateConnection) Send(response, net.Listener) error { return nil }
func (c *certificateConnection) Close() error                      { return nil }

func TestCertificateRequiresExactHTTPSOwnership(t *testing.T) {
	owner := &certificateConnection{identity: "owner"}
	foreign := &certificateConnection{identity: "foreign"}
	bundle := &CertificateBundle{CertificatePEM: []byte("cached owned leaf")}
	g := &guardServer{aliases: map[controlConn]map[string]string{owner: {"app.pprbt": "office.pprbt"}}, leases: map[string]*guardedLease{"socket": {hostname: "office.pprbt", port: 443, owner: owner}}, certificates: map[controlConn]map[string]cachedCertificate{owner: {"app.pprbt": {bundle: bundle, renewAt: time.Now().Add(time.Hour)}}}}
	if got, err := g.certificate(context.Background(), owner, "owner", "app.pprbt"); err != nil || got != bundle {
		t.Fatalf("owned cached leaf: %v", err)
	}
	for _, test := range []struct {
		connection controlConn
		hostname   string
	}{{foreign, "app.pprbt"}, {owner, "office2.pprbt"}, {owner, "app..office.pprbt"}, {owner, "*.office.pprbt"}, {owner, "app.office.pprbt.evil"}} {
		if _, err := g.certificate(context.Background(), test.connection, "owner", test.hostname); err == nil {
			t.Fatalf("issued unauthorized leaf %s", test.hostname)
		}
	}
	g.leases["socket"].port = 80
	if _, err := g.certificate(context.Background(), owner, "owner", "app.pprbt"); err == nil {
		t.Fatal("cached leaf bypassed listener withdrawal")
	}
}

func TestCertificateRenewAtFollowsShortestChainLifetime(t *testing.T) {
	ca, err := splitdns.LoadOrCreateConstrainedCA(t.TempDir(), "pprbt")
	if err != nil {
		t.Fatal(err)
	}
	certPEM, keyPEM, err := ca.IssueCertificate([]string{"office.pprbt"})
	if err != nil {
		t.Fatal(err)
	}
	bundle := &CertificateBundle{CertificatePEM: certPEM, PrivateKeyPEM: keyPEM, RootCAPEM: ca.CertPEM()}
	now := time.Now()
	renewAt, err := certificateRenewAt(bundle, now)
	if err != nil {
		t.Fatal(err)
	}
	leafBlock, _ := pem.Decode(certPEM)
	leaf, _ := x509.ParseCertificate(leafBlock.Bytes)
	if renewAt.After(now.Add(24*time.Hour)) || renewAt.After(leaf.NotAfter.Add(-time.Hour)) {
		t.Fatalf("renewAt=%s leaf expiry=%s", renewAt, leaf.NotAfter)
	}

	bad := *bundle
	bad.RootCAPEM = certPEM
	if _, err := certificateRenewAt(&bad, now); err == nil {
		t.Fatal("accepted leaf as root of cached chain")
	}
}

func TestCertificateWaitCancelsWithoutBlockingRegistry(t *testing.T) {
	g := &guardServer{certificateGate: make(chan struct{}, 1)}
	g.certificateGate <- struct{}{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := g.certificate(ctx, &certificateConnection{}, "owner", "office.pprbt"); done <- err }()
	registryFree := make(chan struct{})
	go func() { g.mu.Lock(); g.mu.Unlock(); close(registryFree) }()
	select {
	case <-registryFree:
	case <-time.After(time.Second):
		t.Fatal("certificate wait locked registry")
	}
	cancel()
	select {
	case err := <-done:
		if err != context.Canceled {
			t.Fatalf("cancellation: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("certificate wait ignored cancellation")
	}
}
