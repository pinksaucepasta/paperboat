//go:build windows

package machineguard

import (
	"crypto/x509"
	"encoding/pem"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/splitdns"
	"golang.org/x/sys/windows"
)

func TestWindowsCRLStoreKeepsOneListPerRootAndRemovesOnlyItsOwn(t *testing.T) {
	store, err := windows.CertOpenStore(windows.CERT_STORE_PROV_MEMORY, 0, 0, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CertCloseStore(store, 0)
	count := func() (n int) {
		for previous := uintptr(0); ; n++ {
			if previous, _, _ = certEnumCRLs.Call(uintptr(store), previous); previous == 0 {
				return n
			}
		}
	}
	var roots []*x509.Certificate
	for range 2 {
		ca, err := splitdns.LoadOrCreateConstrainedCA(t.TempDir(), testBrowserDomain)
		if err != nil {
			t.Fatal(err)
		}
		block, _ := pem.Decode(ca.CertPEM())
		root, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			t.Fatal(err)
		}
		roots = append(roots, root)
		for _, at := range []time.Time{time.Now(), time.Now().Add(23*time.Hour + 30*time.Minute)} { // the second is a renewal
			der, err := ca.RevocationList(at)
			if err != nil {
				t.Fatal(err)
			}
			if err := addCRLToStore(store, der); err != nil {
				t.Fatal(err)
			}
		}
	}
	if n := count(); n != 2 {
		t.Fatalf("lists after renewal=%d, want one per root", n)
	}
	for i, root := range roots {
		if err := removeCRLsFromStore(store, root); err != nil {
			t.Fatal(err)
		}
		if n := count(); n != 1-i {
			t.Fatalf("lists after removing root %d=%d", i, n)
		}
	}
}
