//go:build windows

package machineguard

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// The absent-task prerequisite prevents this acceptance from stopping a user
// installation. Product deny rules/identity are intentionally retained; only the
// uploaded test executables and runner task are disposable test artifacts.
func TestWindowsUninstallOwnedLifecycle(t *testing.T) {
	executable := os.Getenv("PAPERBOAT_UNINSTALL_PB")
	if executable == "" {
		t.Skip("explicit elevated Windows uninstall acceptance")
	}
	installed := filepath.Join(os.Getenv("ProgramFiles"), "Paperboat", "MachineGuard", "pb.exe")
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	exists, err := ownedScheduledTask(ctx, installed)
	if err != nil || exists {
		t.Fatal("requires absent guard task baseline", err)
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		if _, e := Uninstall(cleanup); e != nil {
			t.Error("final product deactivation", e)
		}
	}()
	if err = Install(ctx, executable); err != nil {
		t.Fatal("install", err)
	}
	journalPath := filepath.Join(DefaultStateDir, "reservations.json")
	before, beforeErr := os.ReadFile(journalPath)
	if beforeErr != nil && !os.IsNotExist(beforeErr) {
		t.Fatal(beforeErr)
	}
	roots, err := uninstallRoots(DefaultStateDir)
	if err != nil {
		t.Fatal(err)
	}
	trustedBefore := 0
	for _, root := range roots {
		present, e := windowsUninstallRootPresent(root.certificate.Raw)
		if e != nil {
			t.Fatal(e)
		}
		if present {
			trustedBefore++
		}
	}
	if trustedBefore == 0 {
		var random [6]byte
		if _, e := rand.Read(random[:]); e != nil {
			t.Fatal(e)
		}
		for i := range random {
			random[i] = 'a' + random[i]%26
		}
		suffix := "removal" + string(random[:])
		directory := filepath.Join(DefaultStateDir, "certificates", suffix)
		if _, e := os.Lstat(directory); !os.IsNotExist(e) {
			t.Fatal("fixture CA path exists")
		}
		cleanupRoot := createOwnedRootFixture(t, DefaultStateDir, suffix)
		// Remove this exact test-only root before its source identity, even on failure.
		defer func() {
			if e := removeWindowsRoot(cleanupRoot); e != nil {
				t.Error(e)
				return
			}
			if e := os.RemoveAll(directory); e != nil {
				t.Error(e)
			}
		}()
		if e := addWindowsRootFixture(cleanupRoot); e != nil {
			t.Fatal(e)
		}
		roots, err = uninstallRoots(DefaultStateDir)
		if err != nil {
			t.Fatal(err)
		}
		for _, root := range roots {
			present, e := windowsUninstallRootPresent(root.certificate.Raw)
			if e != nil {
				t.Fatal(e)
			}
			if present {
				trustedBefore++
			}
		}
	}
	if trustedBefore == 0 {
		t.Fatal("no actual trusted root available for deletion proof")
	}
	t.Logf("retained root identities=%d; actually trusted roots before removal=%d", len(roots), trustedBefore)
	invalid := filepath.Join(DefaultStateDir, "certificates", "invalid.public")
	if _, err = os.Lstat(invalid); !os.IsNotExist(err) {
		t.Fatal("fixture path already exists")
	}
	if err = os.MkdirAll(invalid, 0700); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(invalid)
	result, err := Uninstall(ctx)
	if err == nil || len(result.Retained) == 0 {
		t.Fatalf("partial result=%+v err=%v", result, err)
	}
	if err = os.Remove(invalid); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		result, err = Uninstall(ctx)
		if err != nil || len(result.Retained) != 3 {
			t.Fatalf("uninstall%d=%+v err=%v", i, result, err)
		}
		exists, err = ownedScheduledTask(ctx, installed)
		if err != nil || exists {
			t.Fatal("access task remains", err)
		}
	}
	for _, root := range roots {
		if present, e := windowsUninstallRootPresent(root.certificate.Raw); e != nil || present {
			t.Fatal("owned root trust survived uninstall", e)
		}
		if _, e := os.Stat(filepath.Join(DefaultStateDir, "certificates", root.suffix)); !os.IsNotExist(e) {
			t.Fatal("retired CA state remains", e)
		}
	}
	after, afterErr := os.ReadFile(journalPath)
	if !bytes.Equal(before, after) || (beforeErr == nil) != (afterErr == nil) {
		t.Fatal("reservation identity changed")
	}
	listener, err := net.Listen("tcp4", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	address := fmt.Sprintf("127.100.254.231:%d", listener.Addr().(*net.TCPAddr).Port)
	if conn, e := net.DialTimeout("tcp4", address, 250*time.Millisecond); e == nil {
		conn.Close()
		t.Fatal("cached address reached wildcard after uninstall")
	}
	if err = Install(ctx, executable); err != nil {
		t.Fatal("reinstall", err)
	}
	client, err := Connect(ctx, DefaultSocket)
	if err != nil {
		t.Fatal("reinstalled guard", err)
	}
	if err = client.ReplaceNames(ctx, nil); err != nil {
		t.Fatal(err)
	}
	client.Close()
}

func addWindowsRootFixture(root ownedRoot) error {
	name, _ := windows.UTF16PtrFromString("ROOT")
	store, err := windows.CertOpenStore(windows.CERT_STORE_PROV_SYSTEM_W, 0, 0, windows.CERT_SYSTEM_STORE_LOCAL_MACHINE|windows.CERT_STORE_OPEN_EXISTING_FLAG, uintptr(unsafe.Pointer(name)))
	if err != nil {
		return err
	}
	defer windows.CertCloseStore(store, 0)
	encoded := root.certificate.Raw
	certificate, err := windows.CertCreateCertificateContext(windows.X509_ASN_ENCODING|windows.PKCS_7_ASN_ENCODING, &encoded[0], uint32(len(encoded)))
	if err != nil {
		return err
	}
	defer windows.CertFreeCertificateContext(certificate)
	var added *windows.CertContext
	if err = windows.CertAddCertificateContextToStore(store, certificate, windows.CERT_STORE_ADD_REPLACE_EXISTING, &added); err != nil {
		return err
	}
	if added != nil {
		return windows.CertFreeCertificateContext(added)
	}
	return nil
}

func windowsUninstallRootPresent(raw []byte) (bool, error) {
	name, _ := windows.UTF16PtrFromString("ROOT")
	store, err := windows.CertOpenStore(windows.CERT_STORE_PROV_SYSTEM_W, 0, 0, windows.CERT_SYSTEM_STORE_LOCAL_MACHINE|windows.CERT_STORE_OPEN_EXISTING_FLAG, uintptr(unsafe.Pointer(name)))
	if err != nil {
		return false, err
	}
	defer windows.CertCloseStore(store, 0)
	var previous *windows.CertContext
	for {
		cert, e := windows.CertEnumCertificatesInStore(store, previous)
		if e != nil {
			if errors.Is(e, windows.Errno(windows.CRYPT_E_NOT_FOUND)) {
				return false, nil
			}
			return false, e
		}
		if cert == nil {
			return false, nil
		}
		previous = cert
		if bytes.Equal(unsafe.Slice(cert.EncodedCert, cert.Length), raw) {
			windows.CertFreeCertificateContext(cert)
			return true, nil
		}
	}
}
