//go:build windows

package machineguard

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"github.com/pinksaucepasta/paperboat/internal/splitdns"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// This gate must run in the same ordinary elevated administrator context as
// pb install, not in the scheduled LocalSystem runtime. It never adds trust.
func TestElevatedAdministratorLocalCAState(t *testing.T) {
	if os.Getenv("PB_TEST_ELEVATED_LOCAL_CA") != "1" {
		t.Skip("opt-in native elevated administrator gate")
	}
	if err := requireInstallerPrivilege(); err != nil {
		t.Fatal(err)
	}
	if err := requirePrivilege(); err == nil {
		t.Fatal("gate must run as elevated administrator, not LocalSystem")
	}
	var nonce [12]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(os.Getenv("ProgramData"), "Paperboat", ".local-ca-installer-test-"+hex.EncodeToString(nonce[:]))
	if err := protectedDirectory(dir, 0700); err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	cfg := Config{StateDir: dir}
	if err := PrepareLocalCARenewal(context.Background(), cfg); err != nil {
		t.Fatalf("administrator maintenance: %v", err)
	}
	caDir := filepath.Join(dir, "certificates", splitdns.BrowserSuffix)
	if err := protectedDirectory(caDir, 0700); err != nil {
		t.Fatal(err)
	}
	ca, err := splitdns.LoadOrCreateConstrainedCA(caDir, splitdns.BrowserSuffix)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ca.RevocationList(time.Now()); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"rootCA.pem", "rootCA-key.pem", "rootCA.crl"} {
		path := filepath.Join(caDir, name)
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := validateOwnedRootStateFile(path, name, info); err != nil {
			t.Fatalf("installer state not acceptable to guard (%s): %v", name, err)
		}
	}
	journal := filepath.Join(dir, "local-ca-renewal.pem")
	if err := writeRenewalJournal(journal, ca.CertPEM()); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(journal)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateOwnedRootStateFile(journal, "local-ca-renewal.pem", info); err != nil {
		t.Fatal(err)
	}
	if _, err := splitdns.LoadOrCreateConstrainedCA(caDir, splitdns.BrowserSuffix); err != nil {
		t.Fatalf("restart cannot recover installer CA: %v", err)
	}
}
