package splitdns

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLocalCRLMatchesIssuedLeafAndSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	ca, err := LoadOrCreateConstrainedCA(dir, BrowserSuffix)
	if err != nil {
		t.Fatal(err)
	}
	rootBefore := append([]byte(nil), ca.CertPEM()...)
	leafPEM, key, err := ca.IssueCertificate([]string{"hp.local.pprbt.dev"})
	if err != nil {
		t.Fatal(err)
	}
	pair, err := tls.X509KeyPair(leafPEM, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	if len(leaf.CRLDistributionPoints) != 1 || leaf.CRLDistributionPoints[0] != "http://"+BrowserGatewayIP+CRLPath(ca.caCert.Raw) {
		t.Fatal("leaf omitted issuer-bound local HTTP distribution point")
	}
	now := time.Now()
	der, err := ca.RevocationList(now)
	if err != nil {
		t.Fatal(err)
	}
	list, err := ValidateCRL(ca.caCert.Raw, der, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(list.RevokedCertificateEntries) != 0 {
		t.Fatal("non-revoked issuer published fictional revocations")
	}
	restarted, err := LoadOrCreateConstrainedCA(dir, BrowserSuffix)
	if err != nil {
		t.Fatal(err)
	}
	same, err := restarted.RevocationList(now)
	if err != nil || !bytes.Equal(same, der) {
		t.Fatal("restart changed valid signed revocation state")
	}
	renewed, err := restarted.RevocationList(now.Add(24 * time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	next, err := ValidateCRL(ca.caCert.Raw, renewed, now.Add(24*time.Hour))
	if err != nil || next.Number.Cmp(list.Number) <= 0 {
		t.Fatal("CRL renewal did not advance durable number")
	}
	if _, err := restarted.RevocationList(now); err == nil {
		t.Fatal("clock regression rolled back revocation state")
	}
	if !bytes.Equal(rootBefore, restarted.CertPEM()) {
		t.Fatal("revocation refresh replaced trusted issuer")
	}
	if _, err := ValidateCRL(ca.caCert.Raw, der, now.Add(25*time.Hour)); err == nil {
		t.Fatal("expired CRL accepted")
	}
	foreign, err := LoadOrCreateConstrainedCA(t.TempDir(), BrowserSuffix)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateCRL(foreign.caCert.Raw, der, now); err == nil {
		t.Fatal("foreign issuer CRL accepted")
	}
	if err := os.WriteFile(filepath.Join(dir, "rootCA.crl"), []byte("corrupt"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.RevocationList(now); err == nil {
		t.Fatal("corrupt persistent revocation state silently replaced")
	}
}

func TestLocalCRLEndpointNeverForwards(t *testing.T) {
	ca, err := LoadOrCreateConstrainedCA(t.TempDir(), BrowserSuffix)
	if err != nil {
		t.Fatal(err)
	}
	forwarded := false
	p, err := NewProxy(ProxyConfig{Routes: map[string]BrowserRoute{"3000.hp.local.pprbt.dev": {Address: netip.MustParseAddr("127.100.0.2"), Port: 3000}}, RevocationList: func(ctx context.Context, path string) ([]byte, []byte, error) {
		der, err := ca.RevocationList(time.Now())
		return ca.caCert.Raw, der, err
	}, DialContext: func(context.Context, string, string) (net.Conn, error) { forwarded = true; return nil, net.ErrClosed }})
	if err != nil {
		t.Fatal(err)
	}
	path := CRLPath(ca.caCert.Raw)
	for _, tc := range []struct {
		method, path string
		status       int
	}{{"GET", path, 200}, {"HEAD", path, 200}, {"POST", path, 405}, {"GET", path + "?x=1", 404}, {"GET", CRLPathPrefix + "bad", 404}} {
		req := httptest.NewRequest(tc.method, "http://127.100.0.1"+tc.path, nil)
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
		if rec.Code != tc.status {
			t.Fatalf("%s %s status%d", tc.method, tc.path, rec.Code)
		}
		if rec.Code == 200 && (rec.Header().Get("Content-Type") != "application/pkix-crl" || rec.Header().Get("Cache-Control") == "") {
			t.Fatal("missing CRL response contract")
		}
		if tc.method == http.MethodGet && rec.Code == 200 {
			if _, err := ValidateCRL(ca.caCert.Raw, rec.Body.Bytes(), time.Now()); err != nil {
				t.Fatal(err)
			}
		}
		if tc.method == http.MethodHead && rec.Body.Len() != 0 {
			t.Fatal("HEAD leaked body")
		}
	}
	if forwarded {
		t.Fatal("certificate status request reached remote application")
	}
}
