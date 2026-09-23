package splitdns

import (
	"bytes"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLocalCAAndCertificateGeneration(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "pb-ca-test-*")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	ca, err := LoadOrCreateConstrainedCA(tempDir, "pprbt")
	if err != nil {
		t.Fatalf("LoadOrCreateCA failed: %v", err)
	}

	if _, err := os.Stat(filepath.Join(tempDir, "rootCA.pem")); err != nil {
		t.Fatalf("rootCA.pem not found: %v", err)
	}

	domains := []string{"homelab.pprbt", "*.homelab.pprbt", "3000.homelab.pprbt", "127.100.0.1"}
	certPEM, keyPEM, err := ca.IssueCertificate(domains)
	if err != nil {
		t.Fatalf("IssueCertificate failed: %v", err)
	}

	tlsCert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("X509KeyPair failed: %v", err)
	}

	// Verify certificate validation with the CA cert pool
	roots := x509.NewCertPool()
	if ok := roots.AppendCertsFromPEM(ca.CertPEM()); !ok {
		t.Fatalf("failed to append root CA to cert pool")
	}

	parsedLeaf, err := x509.ParseCertificate(tlsCert.Certificate[0])
	if err != nil {
		t.Fatalf("ParseCertificate leaf failed: %v", err)
	}

	opts := x509.VerifyOptions{
		Roots:   roots,
		DNSName: "3000.homelab.pprbt",
	}
	if _, err := parsedLeaf.Verify(opts); err != nil {
		t.Fatalf("leaf cert failed verification for 3000.homelab.pprbt: %v", err)
	}

	opts.DNSName = "homelab.pprbt"
	if _, err := parsedLeaf.Verify(opts); err != nil {
		t.Fatalf("leaf cert failed verification for homelab.pprbt: %v", err)
	}
}

func TestLoadOrCreateCARejectsExpiringRootWithoutRotation(t *testing.T) {
	dir := t.TempDir()
	ca, err := LoadOrCreateConstrainedCA(dir, "pprbt")
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(42), Subject: pkix.Name{CommonName: "expiring Paperboat root"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(12 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		PermittedDNSDomainsCritical: true, PermittedDNSDomains: []string{".pprbt"},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &ca.caKey.PublicKey, ca.caKey)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "rootCA.pem")
	expiring := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err = os.WriteFile(path, expiring, 0644); err != nil {
		t.Fatal(err)
	}
	_, err = LoadOrCreateConstrainedCA(dir, "pprbt")
	if err == nil || !strings.Contains(err.Error(), "delete both") {
		t.Fatalf("expected actionable expiry recovery error, got %v", err)
	}
	after, readErr := os.ReadFile(path)
	if readErr != nil || !bytes.Equal(after, expiring) {
		t.Fatalf("expiring trust root was changed: read=%v", readErr)
	}
}

func TestIssueCertificateRejectsRootThatCannotOutliveLeaf(t *testing.T) {
	ca, err := LoadOrCreateConstrainedCA(t.TempDir(), "pprbt")
	if err != nil {
		t.Fatal(err)
	}
	ca.caCert.NotAfter = time.Now().Add(minimumLeafLifetime / 2)
	if _, _, err = ca.IssueCertificate([]string{"office.pprbt"}); err == nil || !strings.Contains(err.Error(), "remove installed trust") {
		t.Fatalf("expected actionable root expiry error, got %v", err)
	}
}

func TestIssueCertificateClampsLeafToRootValidity(t *testing.T) {
	ca, err := LoadOrCreateConstrainedCA(t.TempDir(), "pprbt")
	if err != nil {
		t.Fatal(err)
	}
	ca.caCert.NotAfter = time.Now().UTC().Truncate(time.Second).Add(7 * 24 * time.Hour)
	certPEM, _, err := ca.IssueCertificate([]string{"office.pprbt"})
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(certPEM)
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if !leaf.NotAfter.Equal(ca.caCert.NotAfter) || leaf.NotBefore.Before(ca.caCert.NotBefore) {
		t.Fatalf("leaf validity %s..%s outside root %s..%s", leaf.NotBefore, leaf.NotAfter, ca.caCert.NotBefore, ca.caCert.NotAfter)
	}
}

func TestLoadOrCreateCAPreservesPartialOrMismatchedTrust(t *testing.T) {
	firstDir := t.TempDir()
	first, err := LoadOrCreateConstrainedCA(firstDir, "pprbt")
	if err != nil {
		t.Fatal(err)
	}
	certBefore, _ := os.ReadFile(filepath.Join(firstDir, "rootCA.pem"))
	if err = os.Remove(filepath.Join(firstDir, "rootCA-key.pem")); err != nil {
		t.Fatal(err)
	}
	if _, err = LoadOrCreateConstrainedCA(firstDir, "pprbt"); err == nil {
		t.Fatal("partial CA was silently replaced")
	}
	certAfter, _ := os.ReadFile(filepath.Join(firstDir, "rootCA.pem"))
	if !bytes.Equal(certBefore, certAfter) {
		t.Fatal("partial CA trust root changed")
	}

	secondDir := t.TempDir()
	if _, err = LoadOrCreateConstrainedCA(secondDir, "pprbt"); err != nil {
		t.Fatal(err)
	}
	otherKey, _ := os.ReadFile(filepath.Join(secondDir, "rootCA-key.pem"))
	if err = os.WriteFile(filepath.Join(firstDir, "rootCA-key.pem"), otherKey, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = LoadOrCreateConstrainedCA(firstDir, "pprbt"); err == nil {
		t.Fatal("mismatched CA pair was accepted")
	}
	_ = first
}

func TestLoadOrCreateConstrainedCARejectsWrongOrUnconstrainedRoot(t *testing.T) {
	dir := t.TempDir()
	ca, err := LoadOrCreateConstrainedCA(dir, "pprbt")
	if err != nil {
		t.Fatal(err)
	}
	if !ca.caCert.PermittedDNSDomainsCritical || len(ca.caCert.PermittedDNSDomains) != 1 || ca.caCert.PermittedDNSDomains[0] != ".pprbt" {
		t.Fatalf("constraints=%v critical=%v", ca.caCert.PermittedDNSDomains, ca.caCert.PermittedDNSDomainsCritical)
	}
	if _, err := LoadOrCreateConstrainedCA(dir, "devbox"); err == nil {
		t.Fatal("constrained root was reused for a different suffix")
	}
	unconstrained := t.TempDir()
	if _, err := loadOrCreateCA(unconstrained, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreateConstrainedCA(unconstrained, "pprbt"); err == nil {
		t.Fatal("unconstrained root was accepted for production TLS")
	}
}
