//go:build linux || darwin || windows

package machineguard

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"github.com/pinksaucepasta/paperboat/internal/splitdns"
	"net"
	"testing"
	"time"
)

type certificateConnection struct{ identity string }

func (c *certificateConnection) Identity() (string, error)         { return c.identity, nil }
func (c *certificateConnection) Receive() (request, error)         { return request{}, net.ErrClosed }
func (c *certificateConnection) Send(response, net.Listener) error { return nil }
func (c *certificateConnection) Close() error                      { return nil }
func certificateGuard(t *testing.T) (*guardServer, *certificateConnection) {
	t.Helper()
	ca, err := splitdns.LoadOrCreateConstrainedCA(t.TempDir(), splitdns.BrowserSuffix)
	if err != nil {
		t.Fatal(err)
	}
	owner := &certificateConnection{identity: "owner"}
	return &guardServer{ca: ca, trustReady: true, connections: map[controlConn]string{owner: "owner"}, aliases: map[controlConn]map[string]string{owner: {"3000.hp.local.pprbt.dev": splitdns.BrowserGatewayHostname}}, leases: map[string]*guardedLease{"gateway": {hostname: splitdns.BrowserGatewayHostname, ip: splitdns.BrowserGatewayIP, port: 443, uid: "owner", owner: owner}}}, owner
}
func TestCertificateRequiresExactAliasAndGatewayOwnership(t *testing.T) {
	g, owner := certificateGuard(t)
	bundle, err := g.certificate(t.Context(), owner, "owner", "3000.hp.local.pprbt.dev")
	if err != nil {
		t.Fatal(err)
	}
	base, err := g.certificate(t.Context(), owner, "owner", "hp.local.pprbt.dev")
	if err != nil || base != bundle {
		t.Fatal("base and port did not reuse alias wildcard")
	}
	leafKey, err := tls.X509KeyPair(bundle.CertificatePEM, bundle.PrivateKeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(leafKey.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, host := range []string{"hp.local.pprbt.dev", "3000.hp.local.pprbt.dev"} {
		if leaf.VerifyHostname(host) != nil {
			t.Fatalf("leaf lacks %s", host)
		}
	}
	rootBlock, _ := pem.Decode(bundle.RootCAPEM)
	if _, err := splitdns.ValidateCRL(rootBlock.Bytes, bundle.RevocationListDER, time.Now()); err != nil {
		t.Fatal(err)
	}
	if len(leaf.CRLDistributionPoints) != 1 || leaf.CRLDistributionPoints[0] != "http://127.100.0.1"+splitdns.CRLPath(rootBlock.Bytes) {
		t.Fatal("missing numeric-loopback CRL distribution point")
	}
	foreign := &certificateConnection{identity: "other"}
	for _, tc := range []struct {
		conn        controlConn
		owner, host string
	}{{foreign, "other", "3000.hp.local.pprbt.dev"}, {owner, "other", "3000.hp.local.pprbt.dev"}, {owner, "owner", "5173.hp.local.pprbt.dev"}, {owner, "owner", "other.local.pprbt.dev"}, {owner, "owner", "*.hp.local.pprbt.dev"}, {owner, "owner", "3000.hp.local.pprbt.dev.evil"}, {owner, "owner", splitdns.BrowserGatewayHostname}} {
		if _, err := g.certificate(t.Context(), tc.conn, tc.owner, tc.host); err == nil {
			t.Fatalf("unauthorized certificate: %s", tc.host)
		}
	}
	g.leases["gateway"].port = 80
	if _, err := g.certificate(t.Context(), owner, "owner", "hp.local.pprbt.dev"); err == nil {
		t.Fatal("cached certificate bypassed gateway withdrawal")
	}
}
func TestCertificateCachePrunesWithdrawnAliasesAndRefreshesCRL(t *testing.T) {
	g, owner := certificateGuard(t)
	original, err := g.certificate(t.Context(), owner, "owner", "3000.hp.local.pprbt.dev")
	if err != nil {
		t.Fatal(err)
	}
	cached := g.certificates[owner]["hp.local.pprbt.dev"]
	cached.renewAt = time.Now().Add(-time.Second)
	g.certificates[owner]["hp.local.pprbt.dev"] = cached
	next, err := g.certificate(t.Context(), owner, "owner", "hp.local.pprbt.dev")
	if err != nil || next == original {
		t.Fatal("expired CRL cache was not refreshed")
	}
	delete(g.aliases[owner], "3000.hp.local.pprbt.dev")
	g.pruneCertificates(owner, "owner")
	if len(g.certificates[owner]) != 0 {
		t.Fatal("withdrawn alias retained leaf cache")
	}
}
func TestCertificateWaitCancelsWithoutBlockingRegistry(t *testing.T) {
	g := &guardServer{certificateGate: make(chan struct{}, 1)}
	g.certificateGate <- struct{}{}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := g.certificate(ctx, &certificateConnection{}, "owner", "hp.local.pprbt.dev"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if !g.mu.TryLock() {
		t.Fatal("canceled signing held registry")
	}
	g.mu.Unlock()
}
