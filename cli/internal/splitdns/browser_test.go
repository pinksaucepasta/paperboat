package splitdns

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	"golang.org/x/net/publicsuffix"
)

func TestPublicBrowserNamespaceAndCertificateConstraint(t *testing.T) {
	host, err := BrowserHostname("hp", 6767, BrowserSuffix)
	if err != nil || host != "6767.hp.local.pprbt.dev" || !IsPublicBrowserHostname(host) {
		t.Fatalf("public hostname %q: %v", host, err)
	}
	for _, bad := range []string{"06767.hp.local.pprbt.dev", "0.hp.local.pprbt.dev", "6767.hp.local.pprbt.dev.evil", "6767.hp.pprbt.dev", BrowserGatewayHostname} {
		if IsPublicBrowserHostname(bad) {
			t.Fatalf("accepted %q", bad)
		}
	}
	for _, bad := range []string{"dev", "pprbt.dev", "other.pprbt.dev"} {
		if _, err := ValidateTrustSuffix(bad); err == nil {
			t.Fatalf("accepted trust scope %q", bad)
		}
	}
	ca, err := LoadOrCreateConstrainedCA(t.TempDir(), BrowserSuffix)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(ca.CertPEM())
	for _, name := range []string{host, "api.pprbt.dev", "unrelated.dev"} {
		cert, key, err := ca.IssueCertificate([]string{name})
		if err != nil {
			t.Fatal(err)
		}
		pair, err := tls.X509KeyPair(cert, key)
		if err != nil {
			t.Fatal(err)
		}
		leaf, err := x509.ParseCertificate(pair.Certificate[0])
		if err != nil {
			t.Fatal(err)
		}
		_, err = leaf.Verify(x509.VerifyOptions{DNSName: name, Roots: roots})
		if (err == nil) != (name == host) {
			t.Fatalf("constraint for %q: %v", name, err)
		}
	}
}

func TestBrowserHostsUseSelectedPortDeviceNames(t *testing.T) {
	a, err := BrowserHostname("machine", 3000, "pprbt")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := BrowserHostname("machine", 3001, "pprbt")
	again, _ := BrowserHostname("machine", 3000, "pprbt")
	if a != again || a == b || a != "3000.machine.pprbt" || b != "3001.machine.pprbt" {
		t.Fatalf("bad stable browser names: %s %s", a, b)
	}
	for _, host := range []string{a, b} {
		if site, err := publicsuffix.EffectiveTLDPlusOne(host); err != nil || site != "machine.pprbt" {
			t.Fatalf("selected nested naming site %q: %v", site, err)
		}
	}
	if _, err := BrowserHostname("machine", 3000, "home.pprbt"); err == nil {
		t.Fatal("multi-label suffix accepted")
	}
}

func TestProxyRejectsUnregisteredHostSNIAndCrossSiteAuthorityBeforeDial(t *testing.T) {
	dialed, issued := 0, 0
	p, err := NewProxy(ProxyConfig{Suffix: "pprbt", Routes: map[string]BrowserRoute{"first.pprbt": {Address: netip.MustParseAddr("127.100.1.2"), Port: 3000}}, DialContext: func(context.Context, string, string) (net.Conn, error) {
		dialed++
		return nil, errors.New("unexpected dial")
	}, IssueCertificate: func(context.Context, string) (tls.Certificate, error) {
		issued++
		return tls.Certificate{}, errors.New("unexpected issue")
	}})
	if err != nil {
		t.Fatal(err)
	}
	for _, host := range []string{"unknown.pprbt", "4000.other.pprbt", "3000.first.pprbt", "child.first.pprbt", "first.pprbt.evil"} {
		req := httptest.NewRequest(http.MethodGet, "http://"+host+"/", nil)
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
		if rec.Code != http.StatusMisdirectedRequest {
			t.Fatalf("host %s status %d", host, rec.Code)
		}
		if _, err := p.GetCertificate(&tls.ClientHelloInfo{ServerName: host}); err == nil {
			t.Fatalf("unknown SNI accepted %s", host)
		}
	}
	req := httptest.NewRequest(http.MethodGet, "https://first.pprbt/", nil)
	req.TLS = &tls.ConnectionState{ServerName: "other.pprbt"}
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusMisdirectedRequest || dialed != 0 || issued != 0 {
		t.Fatalf("unregistered authority reached origin/cert: status%d dial%d issue%d", rec.Code, dialed, issued)
	}
}

func TestProxyRebuildsForwardedMetadataFromActualRequest(t *testing.T) {
	observed := make(chan http.Header, 2)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { observed <- r.Header.Clone(); w.WriteHeader(204) }))
	defer backend.Close()
	address := backend.Listener.Addr().(*net.TCPAddr)
	p, err := NewProxy(ProxyConfig{Suffix: "pprbt", Routes: map[string]BrowserRoute{"site.pprbt": {Address: netip.MustParseAddr("127.0.0.1"), Port: address.Port}}, DialContext: (&net.Dialer{}).DialContext})
	if err != nil {
		t.Fatal(err)
	}
	for _, secure := range []bool{false, true} {
		req := httptest.NewRequest(http.MethodGet, "http://site.pprbt/", nil)
		req.Header.Set("Forwarded", "for=attacker;proto=evil")
		req.Header.Set("X-Forwarded-For", "attacker")
		req.Header.Set("X-Forwarded-Host", "attacker.example")
		req.Header.Set("X-Forwarded-Proto", "evil")
		want := "http"
		if secure {
			req.TLS = &tls.ConnectionState{ServerName: "site.pprbt"}
			want = "https"
		}
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
		if rec.Code != 204 {
			t.Fatalf("status=%d", rec.Code)
		}
		headers := <-observed
		if headers.Get("Forwarded") != "" || strings.Contains(headers.Get("X-Forwarded-For"), "attacker") || headers.Get("X-Forwarded-Host") != "site.pprbt" || headers.Get("X-Forwarded-Proto") != want {
			t.Fatalf("spoofed forwarding metadata: %v", headers)
		}
	}
}
