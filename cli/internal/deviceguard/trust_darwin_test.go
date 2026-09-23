//go:build darwin

package deviceguard

import (
	"context"
	"crypto/sha1"
	"crypto/tls"
	"encoding/hex"
	"encoding/pem"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/splitdns"
)

func TestDarwinGuardSystemTrust(t *testing.T) {
	if os.Getenv("PAPERBOAT_GUARD_DARWIN_TEST") != "1" {
		t.Skip("requires authorized macOS system trust target")
	}
	if os.Geteuid() != 0 {
		t.Fatal("root required")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	dir := t.TempDir()
	darwinTrustDirectory = filepath.Join(dir, "trust")
	defer func() { darwinTrustDirectory = filepath.Join(DefaultStateDir, "trusted-ca") }()
	ca, err := splitdns.LoadOrCreateConstrainedCA(filepath.Join(dir, "ca"), "pbguardreview")
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(ca.CertPEM())
	digest := sha1.Sum(block.Bytes)
	fingerprint := strings.ToUpper(hex.EncodeToString(digest[:]))
	// The generated root is unique to this test. Remove that exact certificate,
	// never all certificates bearing a shared display name.
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if out, err := exec.CommandContext(cleanup, "/usr/bin/security", "delete-certificate", "-t", "-Z", fingerprint, "/Library/Keychains/System.keychain").CombinedOutput(); err != nil {
			t.Errorf("remove test trust: %v %s", err, out)
		}
	}()
	if err = installCATrust(ctx, "0", "pbguardreview", ca.CertPEM()); err != nil {
		t.Fatal(err)
	}
	if err = installCATrust(ctx, "0", "pbguardreview", ca.CertPEM()); err != nil {
		t.Fatal("repeat trust", err)
	}
	cert, key, err := ca.IssueCertificate([]string{"office.pbguardreview"})
	if err != nil {
		t.Fatal(err)
	}
	pair, err := tls.X509KeyPair(cert, key)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("trusted-guard")) }), ReadHeaderTimeout: time.Second}
	defer server.Close()
	go server.Serve(tls.NewListener(listener, &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12}))
	port := strings.Split(listener.Addr().String(), ":")[1]
	out, err := exec.CommandContext(ctx, "/usr/bin/curl", "--noproxy", "*", "--fail", "--silent", "--show-error", "--max-time", "5", "--resolve", "office.pbguardreview:"+port+":127.0.0.1", "https://office.pbguardreview:"+port).CombinedOutput()
	if err != nil || string(out) != "trusted-guard" {
		t.Fatalf("system trusted HTTPS: %v %s", err, out)
	}
}

func TestDarwinSystemTrustVerification(t *testing.T) {
	if os.Getenv("PAPERBOAT_GUARD_DARWIN_TEST") != "1" {
		t.Skip("requires macOS system trust target")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	dir := t.TempDir()
	known, err := exec.CommandContext(ctx, "/usr/bin/security", "find-certificate", "-c", "Apple Root CA", "-p", "/System/Library/Keychains/SystemRootCertificates.keychain").Output()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "root.pem")
	if err = os.WriteFile(path, known, 0600); err != nil {
		t.Fatal(err)
	}
	if err = darwinVerifyCATrust(ctx, path); err != nil {
		t.Fatal("known system root rejected", err)
	}
	unknown, err := splitdns.LoadOrCreateConstrainedCA(filepath.Join(dir, "unknown"), "pbguardreview")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, unknown.CertPEM(), 0600); err != nil {
		t.Fatal(err)
	}
	if err = darwinVerifyCATrust(ctx, path); err == nil {
		t.Fatal("untrusted generated root accepted")
	}
}
