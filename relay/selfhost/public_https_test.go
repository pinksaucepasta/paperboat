package selfhost

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestConfigurePublicHTTPSPreservesClaimAndRecoversInvalidReplacement(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	c := Config{Name: "customer", EndpointHost: "edge.customer.test", Listen: "127.0.0.1:8443", Components: []Component{{Capability: "tunnel", TCPPort: 27443, QUICPort: 27444, Region: "test", FailureDomain: "test", CapacityLimit: 1}}}
	if err := Initialize(dir, c); err != nil {
		t.Fatal(err)
	}
	carrierCert, err := os.ReadFile(filepath.Join(dir, "tls.crt"))
	if err != nil {
		t.Fatal(err)
	}
	carrierKey, err := os.ReadFile(filepath.Join(dir, "tls.key"))
	if err != nil {
		t.Fatal(err)
	}
	s, err := load(dir)
	if err != nil {
		t.Fatal(err)
	}
	s.RuntimeReady = true
	s.ClaimHash = "existing-claim"
	if err := save(dir, s); err != nil {
		t.Fatal(err)
	}
	runtimeDir := filepath.Join(dir, "tunnel")
	if err := os.Mkdir(runtimeDir, 0700); err != nil {
		t.Fatal(err)
	}
	deploymentPath := filepath.Join(runtimeDir, "deployment.json")
	if err := writeJSON(deploymentPath, map[string]any{"infrastructure_tls_cert_file": filepath.Join(dir, "tls.crt"), "control_credential_file": "protected.credential"}); err != nil {
		t.Fatal(err)
	}
	makePair := func(host string, expired bool) (string, string) {
		t.Helper()
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		now := time.Now()
		until := now.Add(time.Hour)
		if expired {
			until = now.Add(-time.Minute)
		}
		tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), DNSNames: []string{host}, NotBefore: now.Add(-time.Hour), NotAfter: until, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
		if err != nil {
			t.Fatal(err)
		}
		keyDER, err := x509.MarshalPKCS8PrivateKey(key)
		if err != nil {
			t.Fatal(err)
		}
		pairDir := t.TempDir()
		certPath, keyPath := filepath.Join(pairDir, "https.crt"), filepath.Join(pairDir, "https.key")
		if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0600); err != nil {
			t.Fatal(err)
		}
		return certPath, keyPath
	}
	certPath, keyPath := makePair(c.EndpointHost, false)
	rollback, err := ConfigurePublicHTTPS(dir, certPath, keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := syncPublicHTTPSDeployment(dir); err != nil {
		t.Fatal(err)
	}
	var deployed map[string]any
	raw, err := os.ReadFile(deploymentPath)
	if err != nil || json.Unmarshal(raw, &deployed) != nil {
		t.Fatal("cannot inspect prepared deployment")
	}
	if deployed["public_https_cert_file"] != certPath || deployed["public_https_key_file"] != keyPath || deployed["infrastructure_tls_cert_file"] != filepath.Join(dir, "tls.crt") || deployed["control_credential_file"] != "protected.credential" {
		t.Fatalf("certificate purposes or existing settings changed: %#v", deployed)
	}
	stable, err := os.ReadFile(filepath.Join(dir, "selfhost.json"))
	if err != nil {
		t.Fatal(err)
	}
	wrongCert, wrongKey := makePair("wrong.customer.test", false)
	expiredCert, expiredKey := makePair(c.EndpointHost, true)
	for _, pair := range [][2]string{{wrongCert, wrongKey}, {expiredCert, expiredKey}, {certPath, wrongKey}, {certPath, ""}} {
		if _, err := ConfigurePublicHTTPS(dir, pair[0], pair[1]); err == nil {
			t.Fatal("invalid replacement accepted")
		}
		actual, err := os.ReadFile(filepath.Join(dir, "selfhost.json"))
		if err != nil || !bytes.Equal(actual, stable) {
			t.Fatal("failed replacement changed state")
		}
	}
	if err := os.Chmod(keyPath, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := ConfigurePublicHTTPS(dir, certPath, keyPath); err == nil {
		t.Fatal("publicly readable private key accepted")
	}
	if err := os.Chmod(keyPath, 0600); err != nil {
		t.Fatal(err)
	}
	if err := rollback(); err != nil {
		t.Fatal(err)
	}
	if err := syncPublicHTTPSDeployment(dir); err != nil {
		t.Fatal(err)
	}
	recovered, err := load(dir)
	if err != nil || recovered.Config.PublicHTTPSCertFile != "" || recovered.ClaimHash != "existing-claim" || recovered.PrivateKey != s.PrivateKey {
		t.Fatal("rollback changed claim or identity")
	}
	for path, want := range map[string][]byte{"tls.crt": carrierCert, "tls.key": carrierKey} {
		got, err := os.ReadFile(filepath.Join(dir, path))
		if err != nil || !bytes.Equal(got, want) {
			t.Fatal("browser TLS configuration modified carrier identity")
		}
	}
}
