package splitdns

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"github.com/pinksaucepasta/paperboat/internal/testcert"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestProxyUsesRenewableCertificateProvider(t *testing.T) {
	ca, err := testcert.New()
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	proxy, err := NewProxy(ProxyConfig{Domain: testBrowserDomain, Routes: map[string]BrowserRoute{"3000.studio.local.example.test": {Address: netip.MustParseAddr("127.0.0.1"), Port: 3000}}, DialContext: (&net.Dialer{}).DialContext, IssueCertificate: func(_ context.Context, hostname string) (tls.Certificate, error) {
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
		if _, err := proxy.GetCertificate(&tls.ClientHelloInfo{ServerName: "3000.studio.local.example.test"}); err != nil {
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

	proxy, err := NewProxy(ProxyConfig{Domain: testBrowserDomain,
		HTTPListenAddr:   "127.0.0.1:0",
		HTTPSListenAddr:  "127.0.0.1:0",
		Routes:           map[string]BrowserRoute{"3000.first.local.example.test": {Address: netip.MustParseAddr("127.0.0.1"), Port: port3000}, "3000.second.local.example.test": {Address: netip.MustParseAddr("127.0.0.1"), Port: portNextjs}},
		IssueCertificate: func(_ context.Context, name string) (tls.Certificate, error) { return ca.TLS(name) },
		DialContext:      (&net.Dialer{}).DialContext,
	})
	if err != nil {
		t.Fatalf("NewProxy failed: %v", err)
	}

	// Test 1: Direct port routing (e.g. <port3000>.homelab.pprbt)
	req := httptest.NewRequest("GET", "/", nil)
	req.Host = "3000.first.local.example.test"
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
	if string(body) != "hello from port 3000 (host: 3000.first.local.example.test, proto: http)" {
		t.Fatalf("unexpected body: %s", string(body))
	}

	// Test 2: Service subdomain routing (e.g. 3000.second.local.example.test)
	req2 := httptest.NewRequest("GET", "/", nil)
	req2.Host = "3000.second.local.example.test"
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
		ServerName: "3000.first.local.example.test",
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
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: roots, DNSName: "3000.first.local.example.test"}); err != nil {
		t.Fatalf("leaf verification failed: %v", err)
	}
}

func TestProxyRejectsUnauthorizedDialAndOutOfSuffixCertificate(t *testing.T) {
	if _, err := NewProxy(ProxyConfig{Domain: testBrowserDomain}); err == nil {
		t.Fatal("proxy accepted no authorized dialer")
	}
	ca, err := testcert.New()
	if err != nil {
		t.Fatal(err)
	}
	proxy, err := NewProxy(ProxyConfig{Domain: testBrowserDomain, IssueCertificate: func(_ context.Context, name string) (tls.Certificate, error) { return ca.TLS(name) }, DialContext: (&net.Dialer{}).DialContext})
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
	proxy, err := NewProxy(ProxyConfig{Domain: testBrowserDomain, HTTPListenAddr: listener.Addr().String(), DialContext: (&net.Dialer{}).DialContext})
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
	proxy, err := NewProxy(ProxyConfig{Domain: testBrowserDomain, HTTPListenAddr: httpAddress, HTTPSListenAddr: httpsBlocker.Addr().String(), IssueCertificate: func(_ context.Context, name string) (tls.Certificate, error) { return ca.TLS(name) }, DialContext: (&net.Dialer{}).DialContext})
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

// The HTTP server returns its deadline instead of its listener-close cause
// when an active request prevents graceful shutdown. Proxy owns both failures.
type proxyFailCloseListener struct {
	net.Listener
	once  sync.Once
	cause error
}

func (l *proxyFailCloseListener) Close() error {
	l.once.Do(func() { _ = l.Listener.Close() })
	return l.cause
}

func TestProxyStopRetainsDeadlineAndOwnedListenerFailure(t *testing.T) {
	native := &os.PathError{Op: "release", Path: "PRIVATE_PROXY_LISTENER", Err: syscall.EIO}
	started := make(chan struct{})
	proxy, err := NewProxy(ProxyConfig{Domain: testBrowserDomain, Routes: map[string]BrowserRoute{"3000.studio.local.example.test": {Address: netip.MustParseAddr("127.0.0.1"), Port: 3000}}, IssueCertificate: func(context.Context, string) (tls.Certificate, error) { return tls.Certificate{}, nil }, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}})
	if err != nil {
		t.Fatal(err)
	}
	httpListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer httpListener.Close()
	httpsListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer httpsListener.Close()
	if err := proxy.StartListeners(&proxyFailCloseListener{Listener: httpListener, cause: native}, httpsListener); err != nil {
		t.Fatal(err)
	}
	defer proxy.Stop(context.Background())
	transport := &http.Transport{}
	defer transport.CloseIdleConnections()
	requestDone := make(chan struct{})
	go func() {
		defer close(requestDone)
		request, _ := http.NewRequest(http.MethodGet, "http://"+httpListener.Addr().String()+"/", nil)
		request.Host = "3000.studio.local.example.test"
		response, _ := (&http.Client{Transport: transport, Timeout: time.Second}).Do(request)
		if response != nil {
			response.Body.Close()
		}
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("real proxy request did not reach origin dial")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	err = proxy.Stop(ctx)
	if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, native) {
		t.Fatal("proxy shutdown discarded deadline or original owned listener failure")
	}
	if strings.Contains(err.Error(), "PRIVATE_PROXY_LISTENER") {
		t.Fatal("proxy shutdown exposed private listener error")
	}
	select {
	case <-requestDone:
	case <-time.After(time.Second):
		t.Fatal("forced shutdown left request running")
	}

	if repeated := proxy.Stop(context.Background()); !errors.Is(repeated, native) {
		t.Fatal("idempotent shutdown erased original listener failure")
	}
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "healthy") }))
	defer origin.Close()
	originAddress := origin.Listener.Addr().(*net.TCPAddr)
	fresh, err := NewProxy(ProxyConfig{Domain: testBrowserDomain, Routes: map[string]BrowserRoute{"3000.studio.local.example.test": {Address: netip.MustParseAddr("127.0.0.1"), Port: originAddress.Port}}, IssueCertificate: func(context.Context, string) (tls.Certificate, error) { return tls.Certificate{}, nil }, DialContext: (&net.Dialer{}).DialContext})
	if err != nil {
		t.Fatal(err)
	}
	freshHTTP, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer freshHTTP.Close()
	freshHTTPS, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer freshHTTPS.Close()
	if err := fresh.StartListeners(freshHTTP, freshHTTPS); err != nil {
		t.Fatal(err)
	}
	defer fresh.Stop(context.Background())
	request, _ := http.NewRequest(http.MethodGet, "http://"+freshHTTP.Addr().String()+"/", nil)
	request.Host = "3000.studio.local.example.test"
	response, err := (&http.Client{Transport: transport, Timeout: time.Second}).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, readErr := io.ReadAll(response.Body)
	response.Body.Close()
	if readErr != nil || response.StatusCode != http.StatusOK || string(body) != "healthy" {
		t.Fatal("fresh proxy did not recover after cleanup failure")
	}
	if err := fresh.Stop(context.Background()); err != nil {
		t.Fatal("healthy shutdown reported normal closed-listener teardown")
	}
}

func TestProxyServesMachineHostOnItsPublishedPort(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "backend host=%s", r.Host)
	}))
	defer backend.Close()
	var backendPort int
	fmt.Sscanf(backend.Listener.Addr().String(), "127.0.0.1:%d", &backendPort)
	ca, err := testcert.New()
	if err != nil {
		t.Fatal(err)
	}
	published, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	unpublished, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	publishedPort := published.Addr().(*net.TCPAddr).Port
	unpublishedPort := unpublished.Addr().(*net.TCPAddr).Port
	numeric, err := BrowserHostname("first", publishedPort, testBrowserDomain)
	if err != nil {
		t.Fatal(err)
	}
	proxy, err := NewProxy(ProxyConfig{Domain: testBrowserDomain,
		HTTPListenAddr: "127.0.0.1:0", HTTPSListenAddr: "127.0.0.1:0",
		Routes:           map[string]BrowserRoute{numeric: {Address: netip.MustParseAddr("127.0.0.1"), Port: backendPort}},
		IssueCertificate: func(_ context.Context, name string) (tls.Certificate, error) { return ca.TLS(name) },
		DialContext:      (&net.Dialer{}).DialContext,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := proxy.AddPortListener(publishedPort, published); err == nil {
		t.Fatal("port listener accepted before the proxy started")
	}
	if err := proxy.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = proxy.Stop(context.Background()) })
	for port, listener := range map[int]net.Listener{publishedPort: published, unpublishedPort: unpublished} {
		if err := proxy.AddPortListener(port, listener); err != nil {
			t.Fatal(err)
		}
	}
	if err := proxy.AddPortListener(80, published); err == nil {
		t.Fatal("privileged port accepted")
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(ca.CertPEM())
	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: "first." + testBrowserDomain},
	}}
	get := func(port int) (*http.Response, error) {
		request, err := http.NewRequest(http.MethodGet, fmt.Sprintf("https://127.0.0.1:%d/", port), nil)
		if err != nil {
			return nil, err
		}
		request.Host = fmt.Sprintf("first.%s:%d", testBrowserDomain, port)
		return client.Do(request)
	}
	response, err := get(publishedPort)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK || !strings.HasPrefix(string(body), "backend host=") {
		t.Fatalf("published port: %d %q", response.StatusCode, body)
	}
	response, err = get(unpublishedPort)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("port without a published service answered %d", response.StatusCode)
	}
}
