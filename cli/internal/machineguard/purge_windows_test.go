//go:build windows

package machineguard

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func selfSignedRootDER(t *testing.T, commonName string) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: commonName},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return der
}

func TestIsPaperboatLocalRootMatchesSubjectOnly(t *testing.T) {
	if !isPaperboatLocalRoot(selfSignedRootDER(t, "Paperboat Local Root CA")) {
		t.Fatal("guard root was not recognised")
	}
	if isPaperboatLocalRoot(selfSignedRootDER(t, "Corporate Root")) {
		t.Fatal("foreign root was matched")
	}
	if isPaperboatLocalRoot([]byte("not a certificate")) {
		t.Fatal("unparsable material was matched")
	}
}

func TestSameExecutablePathIgnoresCaseAndCleaning(t *testing.T) {
	if !sameExecutablePath(`C:\Program Files\Paperboat\MachineGuard\pb.exe`, `c:\program files\paperboat\machineguard\.\pb.exe`) {
		t.Fatal("same executable with different case was not matched")
	}
	if sameExecutablePath(`C:\Program Files\Paperboat\users\x\pb.exe`, `C:\Program Files\Paperboat\MachineGuard\pb.exe`) {
		t.Fatal("user executable was matched as the guard")
	}
}

func TestRemoveEmptyDirectoryKeepsSharedContent(t *testing.T) {
	root := t.TempDir()
	shared := filepath.Join(root, "Paperboat")
	if err := os.MkdirAll(filepath.Join(shared, "users"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := removeEmptyDirectory(shared); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(shared); err != nil {
		t.Fatalf("non-empty directory was removed: %v", err)
	}
	empty := filepath.Join(root, "empty")
	if err := os.Mkdir(empty, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := removeEmptyDirectory(empty); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(empty); !os.IsNotExist(err) {
		t.Fatalf("empty directory was kept (err=%v)", err)
	}
	if err := removeEmptyDirectory(filepath.Join(root, "absent")); err != nil {
		t.Fatalf("absent directory: %v", err)
	}
}
