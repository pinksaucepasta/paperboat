//go:build linux || darwin

package machineguard

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/splitdns"
)

func TestBrowserDomainCallerRequiresInvokingUID(t *testing.T) {
	t.Setenv("SUDO_UID", "1000")
	t.Setenv("PAPERBOAT_INVOKING_UID", "2000")
	if err := validateBrowserDomainCaller("1000"); err != nil {
		t.Fatal(err)
	}
	for _, owner := range []string{"0", "2000", "01000", "-1", "foreign"} {
		if validateBrowserDomainCaller(owner) == nil {
			t.Fatalf("accepted owner %s", owner)
		}
	}
}

// This test never imports or removes OS trust. Run the built package test as
// root to exercise the real protected-state ownership checks and atomic writes.
func TestBrowserDomainProtectedLifecycle(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("protected-state lifecycle requires root; OS trust boundary is injected")
	}
	state := t.TempDir()
	cfg := Config{StateDir: state}
	var removed []string
	install := func(context.Context, Config, []byte) error { return nil }
	remove := func(_ context.Context, _ Config, data []byte) error {
		cert, err := parseLocalTrustRoot(data)
		if err == nil {
			removed = append(removed, localTrustDomain(cert))
		}
		return err
	}
	set := func(owner, domain string) [][]byte {
		t.Helper()
		roots, err := applyBrowserDomainTrust(t.Context(), cfg, owner, domain, install, remove)
		if err != nil {
			t.Fatal(err)
		}
		return roots
	}
	set("1000", "mynet.xyz")
	set("1001", "mynet.xyz")
	g := &guardServer{cfg: cfg, trustReady: true}
	if g.runtimeStatus("1000").BrowserDomain != "mynet.xyz" || g.runtimeStatus("1002").BrowserDomain != splitdns.BrowserSuffix {
		t.Fatal("status exposed another owner's domain")
	}
	if got := set("1000", splitdns.BrowserSuffix); len(got) != 0 || len(removed) != 0 {
		t.Fatal("shared root retired while another owner references it")
	}
	if _, err := os.Stat(filepath.Join(state, "certificates", "mynet.xyz", "rootCA.pem")); err != nil {
		t.Fatal(err)
	}
	if got := set("1001", splitdns.BrowserSuffix); len(got) != 1 || len(removed) != 1 {
		t.Fatal("last owner did not retire its custom root")
	}
	if _, err := os.Stat(filepath.Join(state, "certificates", "mynet.xyz")); !os.IsNotExist(err) {
		t.Fatalf("retired CA state remains: %v", err)
	}
	fail := errors.New("trust install unavailable")
	if _, err := applyBrowserDomainTrust(t.Context(), cfg, "1000", "failed.xyz", func(context.Context, Config, []byte) error { return fail }, remove); !errors.Is(err, fail) {
		t.Fatalf("wrong failure %v", err)
	}
	if domain, err := selectedBrowserDomain(state, "1000"); err != nil || domain != splitdns.BrowserSuffix {
		t.Fatalf("failed trust changed selection: %s %v", domain, err)
	}
	if _, err := os.Stat(filepath.Join(state, "certificates", "failed.xyz")); !os.IsNotExist(err) {
		t.Fatalf("failed provisioning leaked CA state: %v", err)
	}
	// Cancellation after a successful trust import still retires newly created
	// owned state and leaves the prior selection unchanged.
	canceled, cancel := context.WithCancel(t.Context())
	_, err := applyBrowserDomainTrust(canceled, cfg, "1000", "canceled.xyz", func(context.Context, Config, []byte) error { cancel(); return nil }, remove)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
	if domain, err := selectedBrowserDomain(state, "1000"); err != nil || domain != splitdns.BrowserSuffix {
		t.Fatal("cancellation published selection")
	}
	if _, err := os.Stat(filepath.Join(state, "certificates", "canceled.xyz")); !os.IsNotExist(err) {
		t.Fatalf("cancellation leaked root state: %v", err)
	}
	// Historical cleanup must retain every referenced namespace, including those
	// belonging to an owner other than the caller performing the installation.
	set("1001", "shared.xyz")
	defaultCA, err := prepareNamespaceCA(state, splitdns.BrowserSuffix, false)
	if err != nil {
		t.Fatal(err)
	}
	stale, err := prepareNamespaceCA(state, "stale.xyz", false)
	if err != nil {
		t.Fatal(err)
	}
	_ = stale
	roots, err := historicalRootsExcept(state, defaultCA.CertPEM())
	if err != nil {
		t.Fatal(err)
	}
	if len(roots) != 1 || roots[0].suffix != "stale.xyz" {
		t.Fatalf("cleanup roots %+v", roots)
	}
	// Changing a public configuration file cannot authorize certificate signing.
	conn := &certificateConnection{identity: "1000"}
	guard := &guardServer{cfg: cfg, connections: map[controlConn]string{conn: "1000"}, aliases: map[controlConn]map[string]string{conn: {"8989.hp.unapproved.xyz": splitdns.BrowserGatewayHostname}}, leases: map[string]*guardedLease{"gateway": {hostname: splitdns.BrowserGatewayHostname, ip: splitdns.BrowserGatewayIP, port: 443, uid: "1000", owner: conn}}}
	if _, err := guard.certificate(t.Context(), conn, "1000", "8989.hp.unapproved.xyz"); err == nil {
		t.Fatal("unapproved namespace signed")
	}
	// The approved owner can sign its numeric route, named route, and base,
	// but another owner cannot borrow the same CA or an unregistered port.
	approvedConn := &certificateConnection{identity: "1001"}
	approvedGuard := &guardServer{cfg: cfg, connections: map[controlConn]string{approvedConn: "1001"}, aliases: map[controlConn]map[string]string{approvedConn: {"8989.hp.shared.xyz": splitdns.BrowserGatewayHostname, "jellyfin.hp.shared.xyz": "8989.hp.shared.xyz"}}, leases: map[string]*guardedLease{"gateway": {hostname: splitdns.BrowserGatewayHostname, ip: splitdns.BrowserGatewayIP, port: 443, uid: "1001", owner: approvedConn}}}
	bundle, err := approvedGuard.certificate(t.Context(), approvedConn, "1001", "jellyfin.hp.shared.xyz")
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(bundle.CertificatePEM)
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	rootBlock, _ := pem.Decode(bundle.RootCAPEM)
	root, err := x509.ParseCertificate(rootBlock.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(root)
	for _, host := range []string{"hp.shared.xyz", "8989.hp.shared.xyz", "jellyfin.hp.shared.xyz"} {
		if _, err := leaf.Verify(x509.VerifyOptions{DNSName: host, Roots: pool}); err != nil {
			t.Fatalf("trusted leaf %s: %v", host, err)
		}
		if _, err := approvedGuard.certificate(t.Context(), approvedConn, "1001", host); err != nil {
			t.Fatalf("authorized host %s: %v", host, err)
		}
	}
	for _, host := range []string{"3000.hp.shared.xyz", "other.hp.shared.xyz", "8989.other.shared.xyz", "8989.hp.local.pprbt.dev"} {
		if _, err := approvedGuard.certificate(t.Context(), approvedConn, "1001", host); err == nil {
			t.Fatalf("signed unauthorized host %s", host)
		}
	}
	if _, err := approvedGuard.certificate(t.Context(), approvedConn, "1000", "jellyfin.hp.shared.xyz"); err == nil {
		t.Fatal("another owner borrowed domain authority")
	}
	delete(approvedGuard.aliases[approvedConn], "8989.hp.shared.xyz")
	if _, err := approvedGuard.certificate(t.Context(), approvedConn, "1001", "jellyfin.hp.shared.xyz"); err == nil {
		t.Fatal("named alias cache survived numeric withdrawal")
	}
	// A trust retirement failure reports the changed selection and preserves
	// the exact old root for installer retry; reversing the selection is safe.
	set("1002", "retry-old.xyz")
	unavailable := errors.New("trust retirement unavailable")
	if _, err := applyBrowserDomainTrust(t.Context(), cfg, "1002", "retry-next.xyz", install, func(context.Context, Config, []byte) error { return unavailable }); !errors.Is(err, unavailable) {
		t.Fatalf("retirement failure: %v", err)
	}
	if domain, err := selectedBrowserDomain(state, "1002"); err != nil || domain != "retry-next.xyz" {
		t.Fatal("retirement failure hid changed selection")
	}
	if _, err := os.Stat(filepath.Join(state, "certificates", "retry-old.xyz", "rootCA.pem")); err != nil {
		t.Fatal("retirement failure lost cleanup identity")
	}
	if roots := set("1002", "retry-old.xyz"); len(roots) != 1 {
		t.Fatal("rollback did not retire replacement root")
	}
	// Corrupt protected selections fail closed instead of falling back to another
	// owner's approved domain or creating new trust material.
	if err := os.WriteFile(filepath.Join(state, "browser-domains.json"), []byte(`{"schema":"foreign"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if guard.runtimeStatus("1000").TrustReady || guard.certificateRequestAuthorizedDomain(conn, "1000", "8989.hp.shared.xyz", "hp", "shared.xyz") {
		t.Fatal("malformed selection remained authorized")
	}
}
