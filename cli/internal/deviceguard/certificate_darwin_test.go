//go:build darwin

package deviceguard

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/splitdns"
)

func TestDarwinFreshCertificateDoesNotRequireSystemTrust(t *testing.T) {
	if os.Getenv("PAPERBOAT_DARWIN_CERTIFICATE_TEST") != "1" {
		t.Skip("requires an authorized root macOS certificate fixture")
	}
	if os.Geteuid() != 0 {
		t.Fatal("protected CA fixture requires root")
	}
	root := t.TempDir()
	previous := darwinTrustDirectory
	darwinTrustDirectory = filepath.Join(root, "system-trust")
	t.Cleanup(func() { darwinTrustDirectory = previous })
	owner := &certificateConnection{identity: "501"}
	foreign := &certificateConnection{identity: "502"}
	hostname := "6768.hp." + splitdns.BrowserSuffix
	g := &guardServer{
		cfg: Config{StateDir: filepath.Join(root, "state")},
		leases: map[string]*guardedLease{"https": {
			hostname: splitdns.BrowserGatewayHostname, port: 443, owner: owner,
		}},
		aliases: map[controlConn]map[string]string{owner: {hostname: splitdns.BrowserGatewayHostname}},
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	bundle, err := g.certificate(ctx, owner, "501", hostname)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(darwinTrustDirectory); !os.IsNotExist(err) {
		t.Fatalf("background issuance attempted system trust enrollment: %v", err)
	}
	if _, err := tls.X509KeyPair(bundle.CertificatePEM, bundle.PrivateKeyPEM); err != nil {
		t.Fatal("leaf and private key do not match", err)
	}
	leafBlock, _ := pem.Decode(bundle.CertificatePEM)
	rootBlock, _ := pem.Decode(bundle.RootCAPEM)
	if leafBlock == nil || rootBlock == nil {
		t.Fatal("missing real certificate chain")
	}
	leaf, err := x509.ParseCertificate(leafBlock.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := x509.ParseCertificate(rootBlock.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if !ca.IsCA || !ca.PermittedDNSDomainsCritical || len(ca.PermittedDNSDomains) != 1 || ca.PermittedDNSDomains[0] != "."+splitdns.BrowserSuffix {
		t.Fatal("browser CA constraints changed")
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: roots, DNSName: hostname}); err != nil {
		t.Fatal("genuine owned leaf signature or name invalid", err)
	}
	if leaf.VerifyHostname("6768.foreign."+splitdns.BrowserSuffix) == nil {
		t.Fatal("issued leaf admits a foreign hostname")
	}
	if _, err := g.certificate(ctx, foreign, "502", hostname); err == nil {
		t.Fatal("foreign owner received a certificate")
	}
	delete(g.certificates, owner)
	delete(g.leases, "https")
	if _, err := g.certificate(ctx, owner, "501", hostname); err == nil {
		t.Fatal("withdrawn owner received a fresh certificate")
	}
}
