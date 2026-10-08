package splitdns

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"github.com/pinksaucepasta/paperboat/internal/testcert"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"testing"
)

func TestProxyUsesRenewableCertificateProvider(t *testing.T) {
	ca, err := testcert.New()
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	proxy, err := NewProxy(ProxyConfig{Routes: map[string]BrowserRoute{"3000.studio.local.pprbt.dev": {Address: netip.MustParseAddr("127.0.0.1"), Port: 3000}}, DialContext: (&net.Dialer{}).DialContext, IssueCertificate: func(_ context.Context, hostname string) (tls.Certificate, error) {
		calls++
		certificate, key, issueErr := ca.IssueCertificate([]string{hostname})
		if issueErr != nil {
			return tls.Certificate{}, issueErr
		}
		return tls.X509KeyPair(certificate, key)
	}})
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := proxy.GetCertificate(&tls.ClientHelloInfo{ServerName: "3000.studio.local.pprbt.dev"}); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 2 {
		t.Fatalf("certificate callback calls=%d want=2", calls)
	}
}

func TestProxySubdomainRoutingAndTLS(t *testing.T) {
	// 1. Backend server on custom port 3000
	backend3000 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Backend", "port-3000")
		fmt.Fprintf(w, "hello from port 3000 (host: %s, proto: %s)", r.Host, r.Header.Get("X-Forwarded-Proto"))
	}))
	defer backend3000.Close()

	// Backend server on custom port 8080 (for service "nextjs")
	backendNextjs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Backend", "nextjs-service")
		fmt.Fprintf(w, "hello from nextjs (host: %s)", r.Host)
	}))
	defer backendNextjs.Close()

	var port3000, portNextjs int
	fmt.Sscanf(backend3000.Listener.Addr().String(), "127.0.0.1:%d", &port3000)
	fmt.Sscanf(backendNextjs.Listener.Addr().String(), "127.0.0.1:%d", &portNextjs)

	tempDir, err := os.MkdirTemp("", "pb-proxy-test-*")
	if err != nil {
		t.Fatalf("mkdir temp: %v", err)
	}
	defer os.RemoveAll(tempDir)

	ca, err := testcert.New()
	if err != nil {
		t.Fatalf("load CA: %v", err)
	}

	proxy, err := NewProxy(ProxyConfig{
		HTTPListenAddr:   "127.0.0.1:0",
		HTTPSListenAddr:  "127.0.0.1:0",
		Routes:           map[string]BrowserRoute{"3000.first.local.pprbt.dev": {Address: netip.MustParseAddr("127.0.0.1"), Port: port3000}, "3000.second.local.pprbt.dev": {Address: netip.MustParseAddr("127.0.0.1"), Port: portNextjs}},
		IssueCertificate: func(_ context.Context, name string) (tls.Certificate, error) { return ca.TLS(name) },
		DialContext:      (&net.Dialer{}).DialContext,
	})
	if err != nil {
		t.Fatalf("NewProxy failed: %v", err)
	}

	// Test 1: Direct port routing (e.g. <port3000>.homelab.pprbt)
	req := httptest.NewRequest("GET", "/", nil)
	req.Host = "3000.first.local.pprbt.dev"
	rec := httptest.NewRecorder()
	proxy.ServeHTTP(rec, req)

	resp := rec.Result()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", resp.StatusCode)
	}
	if resp.Header.Get("X-Backend") != "port-3000" {
		t.Fatalf("expected backend port-3000, got %s", resp.Header.Get("X-Backend"))
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "hello from port 3000 (host: 3000.first.local.pprbt.dev, proto: http)" {
		t.Fatalf("unexpected body: %s", string(body))
	}

	// Test 2: Service subdomain routing (e.g. 3000.second.local.pprbt.dev)
	req2 := httptest.NewRequest("GET", "/", nil)
	req2.Host = "3000.second.local.pprbt.dev"
	rec2 := httptest.NewRecorder()
	proxy.ServeHTTP(rec2, req2)

	resp2 := rec2.Result()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", resp2.StatusCode)
	}
	if resp2.Header.Get("X-Backend") != "nextjs-service" {
		t.Fatalf("expected backend nextjs-service, got %s", resp2.Header.Get("X-Backend"))
	}

	// Test 3: Dynamic TLS certificate generation via GetCertificate
	hello := &tls.ClientHelloInfo{
		ServerName: "3000.first.local.pprbt.dev",
	}
	cert, err := proxy.GetCertificate(hello)
	if err != nil {
		t.Fatalf("GetCertificate failed: %v", err)
	}

	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(ca.CertPEM())
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatalf("parse leaf: %v", err)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: roots, DNSName: "3000.first.local.pprbt.dev"}); err != nil {
		t.Fatalf("leaf verification failed: %v", err)
	}
}

func TestProxyRejectsUnauthorizedDialAndOutOfSuffixCertificate(t *testing.T) {
	if _, err := NewProxy(ProxyConfig{}); err == nil {
		t.Fatal("proxy accepted no authorized dialer")
	}
	ca, err := testcert.New()
	if err != nil {
		t.Fatal(err)
	}
	proxy, err := NewProxy(ProxyConfig{IssueCertificate: func(_ context.Context, name string) (tls.Certificate, error) { return ca.TLS(name) }, DialContext: (&net.Dialer{}).DialContext})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = proxy.GetCertificate(&tls.ClientHelloInfo{ServerName: "attacker.example"}); err == nil {
		t.Fatal("certificate issued outside active suffix")
	}
}

func TestProxyStartReportsBindFailureSynchronously(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	proxy, err := NewProxy(ProxyConfig{HTTPListenAddr: listener.Addr().String(), DialContext: (&net.Dialer{}).DialContext})
	if err != nil {
		t.Fatal(err)
	}
	if err = proxy.Start(); err == nil {
		t.Fatal("occupied HTTP listener reported ready")
	}
}

func TestProxyStartRollsBackHTTPWhenHTTPSBindFails(t *testing.T) {
	httpReservation, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	httpAddress := httpReservation.Addr().String()
	_ = httpReservation.Close()
	httpsBlocker, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer httpsBlocker.Close()
	ca, err := testcert.New()
	if err != nil {
		t.Fatal(err)
	}
	proxy, err := NewProxy(ProxyConfig{HTTPListenAddr: httpAddress, HTTPSListenAddr: httpsBlocker.Addr().String(), IssueCertificate: func(_ context.Context, name string) (tls.Certificate, error) { return ca.TLS(name) }, DialContext: (&net.Dialer{}).DialContext})
	if err != nil {
		t.Fatal(err)
	}
	if err = proxy.Start(); err == nil {
		t.Fatal("occupied HTTPS listener reported ready")
	}
	rebound, err := net.Listen("tcp", httpAddress)
	if err != nil {
		t.Fatalf("HTTP listener leaked after partial startup: %v", err)
	}
	_ = rebound.Close()
}
