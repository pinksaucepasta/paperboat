package splitdns

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	"golang.org/x/net/publicsuffix"
)

func TestBrowserNamespaceRejectsForeignNames(t *testing.T) {
	host, err := BrowserHostname("hp", 6767, testBrowserDomain)
	if err != nil {
		t.Fatalf("public hostname %q: %v", host, err)
	}
	if machine, label, err := ParseBrowserHostname(host, testBrowserDomain); err != nil || machine != "hp" || label != "6767" {
		t.Fatalf("parsed %q = %q, %q: %v", host, machine, label, err)
	}
	for _, bad := range []string{"06767.hp.local.example.test", "0.hp.local.example.test", "6767.hp.local.example.test.evil", "6767.hp.example.test", BrowserGatewayHostname} {
		if _, _, err := ParseBrowserHostname(bad, testBrowserDomain); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
}

func TestBrowserHostsUseSelectedPortMachineNames(t *testing.T) {
	a, err := BrowserHostname("machine", 3000, testBrowserDomain)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := BrowserHostname("machine", 3001, testBrowserDomain)
	again, _ := BrowserHostname("machine", 3000, testBrowserDomain)
	if a != again || a == b || a != "3000.machine.local.example.test" || b != "3001.machine.local.example.test" {
		t.Fatalf("bad stable browser names: %s %s", a, b)
	}
	for _, host := range []string{a, b} {
		if site, err := publicsuffix.EffectiveTLDPlusOne(host); err != nil || site != "example.test" {
			t.Fatalf("selected nested naming site %q: %v", site, err)
		}
	}
	if host, err := BrowserHostname("machine", 3000, "mynet.xyz"); err != nil || host != "3000.machine.mynet.xyz" {
		t.Fatalf("custom hostname %q: %v", host, err)
	}
	if _, err := BrowserHostname("machine", 3000, "co.uk"); err == nil {
		t.Fatal("public suffix accepted")
	}
}

func TestProxyRejectsUnregisteredHostSNIAndCrossSiteAuthorityBeforeDial(t *testing.T) {
	dialed, issued := 0, 0
	p, err := NewProxy(ProxyConfig{Domain: testBrowserDomain, Routes: map[string]BrowserRoute{"3000.first.local.example.test": {Address: netip.MustParseAddr("127.100.1.2"), Port: 3000}}, DialContext: func(context.Context, string, string) (net.Conn, error) {
		dialed++
		return nil, errors.New("unexpected dial")
	}, IssueCertificate: func(context.Context, string) (tls.Certificate, error) {
		issued++
		return tls.Certificate{}, errors.New("unexpected issue")
	}})
	if err != nil {
		t.Fatal(err)
	}
	for _, host := range []string{"unknown.pprbt", "4000.other.pprbt", "3000.3000.first.local.example.test", "child.3000.first.local.example.test", "3000.first.local.example.test.evil"} {
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
	req := httptest.NewRequest(http.MethodGet, "https://3000.first.local.example.test/", nil)
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
	p, err := NewProxy(ProxyConfig{Domain: testBrowserDomain, Routes: map[string]BrowserRoute{"3000.site.local.example.test": {Address: netip.MustParseAddr("127.0.0.1"), Port: address.Port}}, DialContext: (&net.Dialer{}).DialContext})
	if err != nil {
		t.Fatal(err)
	}
	for _, secure := range []bool{false, true} {
		req := httptest.NewRequest(http.MethodGet, "http://3000.site.local.example.test/", nil)
		req.Header.Set("Forwarded", "for=attacker;proto=evil")
		req.Header.Set("X-Forwarded-For", "attacker")
		req.Header.Set("X-Forwarded-Host", "attacker.example")
		req.Header.Set("X-Forwarded-Proto", "evil")
		want := "http"
		if secure {
			req.TLS = &tls.ConnectionState{ServerName: "3000.site.local.example.test"}
			want = "https"
		}
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
		if rec.Code != 204 {
			t.Fatalf("status=%d", rec.Code)
		}
		headers := <-observed
		if headers.Get("Forwarded") != "" || strings.Contains(headers.Get("X-Forwarded-For"), "attacker") || headers.Get("X-Forwarded-Host") != "3000.site.local.example.test" || headers.Get("X-Forwarded-Proto") != want {
			t.Fatalf("spoofed forwarding metadata: %v", headers)
		}
	}
}

// Applying local browser configuration can stop a newly created gateway before
// its serving goroutines have been scheduled.
func TestProxyImmediateStopAndRestartKeepsListenerOwnership(t *testing.T) {
	p, err := NewProxy(ProxyConfig{Domain: testBrowserDomain, HTTPListenAddr: "127.0.0.1:0", HTTPSListenAddr: "127.0.0.1:0", DialContext: (&net.Dialer{}).DialContext, IssueCertificate: func(context.Context, string) (tls.Certificate, error) { return tls.Certificate{}, errors.New("unused") }})
	if err != nil {
		t.Fatal(err)
	}
	for range 30 {
		if err := p.Start(); err != nil {
			t.Fatal(err)
		}
		if err := p.Stop(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
}
