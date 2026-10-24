package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"flag"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/relay/selfhost"
)

func writeTestCertificate(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "node.example.test"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func TestStageTrustFileCopiesOwnerOnlyIntoStateDirectory(t *testing.T) {
	source := t.TempDir()
	state := t.TempDir()
	cert := filepath.Join(source, "fullchain.pem")
	writeTestCertificate(t, cert, 0644)
	key := filepath.Join(source, "privkey.pem")
	if err := os.WriteFile(key, []byte("key material"), 0600); err != nil {
		t.Fatal(err)
	}
	staged, err := stageTrustFile(state, publicCertFile, cert)
	if err != nil {
		t.Fatalf("certificate was not staged: %v", err)
	}
	if filepath.Dir(staged) != state {
		t.Fatalf("certificate staged outside the protected state directory: %s", staged)
	}
	info, err := os.Stat(staged)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("staged certificate is not owner-only: %v %v", info.Mode().Perm(), err)
	}
	original, _ := os.ReadFile(cert)
	copied, _ := os.ReadFile(staged)
	if string(original) != string(copied) {
		t.Fatal("staged certificate content differs from the source")
	}
	again, err := stageTrustFile(state, publicCertFile, cert)
	if err != nil || again != staged {
		t.Fatalf("restaging the same content changed the copy: %s %v", again, err)
	}
	stagedKey, err := stageTrustFile(state, publicKeyFile, key)
	if err != nil {
		t.Fatalf("private key was not staged: %v", err)
	}
	if info, err := os.Stat(stagedKey); err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("staged private key is not owner-only: %v", err)
	}
}

func TestStageTrustFileErrorsNameTheFile(t *testing.T) {
	state := t.TempDir()
	missing := filepath.Join(t.TempDir(), "missing-fullchain.pem")
	directory := t.TempDir()
	for _, src := range []string{missing, directory} {
		_, err := stageTrustFile(state, publicCertFile, src)
		if err == nil || !strings.Contains(err.Error(), src) {
			t.Fatalf("read error does not name %s: %v", src, err)
		}
	}
	key := filepath.Join(t.TempDir(), "privkey.pem")
	if err := os.WriteFile(key, []byte("key"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := stageTrustFile(state, publicKeyFile, key); err == nil || !strings.Contains(err.Error(), key) || !strings.Contains(err.Error(), "owner-only") {
		t.Fatalf("group-readable key error does not name the file and rule: %v", err)
	}
	notCertificate := filepath.Join(t.TempDir(), "fullchain.pem")
	if err := os.WriteFile(notCertificate, []byte("not pem"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := stageTrustFile(state, publicCertFile, notCertificate); err == nil || !strings.Contains(err.Error(), notCertificate) {
		t.Fatalf("non-certificate error does not name the file: %v", err)
	}
}

func TestStageInstallFilesPointsConfigurationAtCopies(t *testing.T) {
	source := t.TempDir()
	cert := filepath.Join(source, "fullchain.pem")
	writeTestCertificate(t, cert, 0644)
	key := filepath.Join(source, "privkey.pem")
	if err := os.WriteFile(key, []byte("key"), 0600); err != nil {
		t.Fatal(err)
	}
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	fs.String("https-cert", "", "")
	fs.String("https-key", "", "")
	fs.String("control-ca", "", "")
	if err := fs.Parse([]string{"--https-cert", cert, "--https-key", key, "--control-ca", cert}); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(t.TempDir(), "state")
	c, err := stageInstallFiles(state, fs, selfhost.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(c.PublicHTTPSCertFile) != state || filepath.Dir(c.PublicHTTPSKeyFile) != state {
		t.Fatalf("configuration still points outside the state directory: %s %s", c.PublicHTTPSCertFile, c.PublicHTTPSKeyFile)
	}
	if filepath.Dir(c.ControlCA) != state {
		t.Fatalf("control CA not staged into the state directory: %q", c.ControlCA)
	}
}

func TestStageInstallFilesDefaultsToPublicRoots(t *testing.T) {
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	fs.String("https-cert", "", "")
	fs.String("https-key", "", "")
	fs.String("control-ca", "", "")
	if err := fs.Parse(nil); err != nil {
		t.Fatal(err)
	}
	c, err := stageInstallFiles(filepath.Join(t.TempDir(), "state"), fs, selfhost.Config{})
	if err != nil || c.ControlCA != "" {
		t.Fatalf("without --control-ca the control plane must use public roots: %q %v", c.ControlCA, err)
	}
}

func TestStageInstallFilesRefusesControlCAThatIsNotACertificate(t *testing.T) {
	bad := filepath.Join(t.TempDir(), "control-ca.pem")
	if err := os.WriteFile(bad, []byte("not a certificate"), 0644); err != nil {
		t.Fatal(err)
	}
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	fs.String("https-cert", "", "")
	fs.String("https-key", "", "")
	fs.String("control-ca", "", "")
	if err := fs.Parse([]string{"--control-ca", bad}); err != nil {
		t.Fatal(err)
	}
	_, err := stageInstallFiles(filepath.Join(t.TempDir(), "state"), fs, selfhost.Config{})
	if err == nil || !strings.Contains(err.Error(), bad) || !strings.Contains(err.Error(), "PEM certificate") {
		t.Fatalf("invalid control CA error does not name the file: %v", err)
	}
}

func TestMatchStoredTrustFileAcceptsOnlyStagedContent(t *testing.T) {
	source := t.TempDir()
	cert := filepath.Join(source, "fullchain.pem")
	writeTestCertificate(t, cert, 0644)
	state := t.TempDir()
	staged, err := stageTrustFile(state, publicCertFile, cert)
	if err != nil {
		t.Fatal(err)
	}
	if err := matchStoredTrustFile(state, publicCertFile, "https-cert", cert, staged); err != nil {
		t.Fatalf("repeated flag with the staged content was refused: %v", err)
	}
	other := filepath.Join(source, "renewed.pem")
	writeTestCertificate(t, other, 0644)
	if err := matchStoredTrustFile(state, publicCertFile, "https-cert", other, staged); err == nil || !strings.Contains(err.Error(), "existing --https-cert differs") {
		t.Fatalf("changed certificate was accepted without configure-tls: %v", err)
	}
}
