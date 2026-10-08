//go:build linux || darwin || windows

package machineguard

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"github.com/pinksaucepasta/paperboat/internal/splitdns"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func renewalFixture(t *testing.T, days int, namespace ...string) (string, string, []byte) {
	t.Helper()
	directory := t.TempDir()
	domain := splitdns.BrowserSuffix
	if len(namespace) > 0 {
		domain = namespace[0]
	}
	ca, err := splitdns.LoadOrCreateConstrainedCA(directory, domain)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM, err := os.ReadFile(filepath.Join(directory, "rootCA-key.pem"))
	if err != nil {
		t.Fatal(err)
	}
	keyBlock, _ := pem.Decode(keyPEM)
	key, err := x509.ParsePKCS1PrivateKey(keyBlock.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(ca.CertPEM())
	root, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	root.NotAfter = time.Now().Add(time.Duration(days) * 24 * time.Hour)
	der, err := x509.CreateCertificate(rand.Reader, root, root, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	public := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := os.WriteFile(filepath.Join(directory, "rootCA.pem"), public, 0644); err != nil {
		t.Fatal(err)
	}
	return directory, filepath.Join(t.TempDir(), "local-ca-renewal.pem"), public
}
func validateFixtureRenewal(path, name string, info os.FileInfo) error {
	if !info.Mode().IsRegular() {
		return errors.New("unsafe fixture file")
	}
	return nil
}
func TestLocalCARenewalRetriesExactTrustRemovalBeforeStateDeletion(t *testing.T) {
	directory, journal, root := renewalFixture(t, 10)
	unavailable := errors.New("trust unavailable")
	if err := renewLocalCA(t.Context(), directory, journal, time.Now(), validateFixtureRenewal, func(public []byte) error {
		if !bytes.Equal(public, root) {
			t.Fatal("wrong root retired")
		}
		return unavailable
	}); !errors.Is(err, unavailable) {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(directory, "rootCA-key.pem")); err != nil {
		t.Fatal("failure deleted working CA key")
	}
	if _, err := os.Stat(journal); err != nil {
		t.Fatal("retry journal missing")
	}
	// Simulate interruption after some old owned files were deleted.
	if err := os.Remove(filepath.Join(directory, "rootCA-key.pem")); err != nil {
		t.Fatal(err)
	}
	if err := renewLocalCA(t.Context(), directory, journal, time.Now(), validateFixtureRenewal, func(public []byte) error {
		if !bytes.Equal(public, root) {
			t.Fatal("retry lost root fingerprint")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{journal, filepath.Join(directory, "rootCA.pem"), filepath.Join(directory, "rootCA-key.pem")} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatal("old CA state retained after successful retirement")
		}
	}
	if _, err := splitdns.LoadOrCreateConstrainedCA(directory, splitdns.BrowserSuffix); err != nil {
		t.Fatal("renewal cannot recreate a usable issuer:", err)
	}
}
func TestLocalCARenewalPreservesHealthyAndForeignState(t *testing.T) {
	directory, journal, _ := renewalFixture(t, 60)
	called := false
	if err := renewLocalCA(t.Context(), directory, journal, time.Now(), validateFixtureRenewal, func([]byte) error { called = true; return nil }); err != nil || called {
		t.Fatal("healthy root rotated")
	}
	directory, journal, _ = renewalFixture(t, 10)
	if err := os.WriteFile(journal, []byte("foreign"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := renewLocalCA(context.Background(), directory, journal, time.Now(), validateFixtureRenewal, func([]byte) error { t.Fatal("foreign journal reached trust removal"); return nil }); err == nil {
		t.Fatal("foreign renewal journal accepted")
	}
	if _, err := os.Stat(filepath.Join(directory, "rootCA-key.pem")); err != nil {
		t.Fatal("foreign journal destroyed key")
	}
}

func TestCustomBrowserCARenewalRetainsExactNamespace(t *testing.T) {
	directory, journal, old := renewalFixture(t, 10, "mynet.xyz")
	if err := renewLocalCA(t.Context(), directory, journal, time.Now(), validateFixtureRenewal, func(public []byte) error {
		if !bytes.Equal(public, old) {
			t.Fatal("custom renewal removed a different root")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	renewed, err := splitdns.LoadOrCreateConstrainedCA(directory, "mynet.xyz")
	if err != nil {
		t.Fatal(err)
	}
	root, err := parseLocalTrustRoot(renewed.CertPEM())
	if err != nil {
		t.Fatal(err)
	}
	if localTrustDomain(root) != "mynet.xyz" || bytes.Equal(renewed.CertPEM(), old) {
		t.Fatal("custom renewal changed scope or retained expiring root")
	}
	if _, err := splitdns.LoadConstrainedCA(directory, splitdns.BrowserSuffix); err == nil {
		t.Fatal("renewed root crossed namespace")
	}
}
