package splitdns

import (
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"testing"
)

const httpsTestHost = "443.homelab." + testBrowserDomain

func httpsTestRoute(t *testing.T, server *httptest.Server, https bool) BrowserRoute {
	t.Helper()
	_, port, _ := net.SplitHostPort(server.Listener.Addr().String())
	n, _ := strconv.Atoi(port)
	return BrowserRoute{Address: netip.MustParseAddr("127.0.0.1"), Port: n, HTTPS: https}
}

func serveThrough(t *testing.T, routes map[string]BrowserRoute, host, path string) *httptest.ResponseRecorder {
	t.Helper()
	proxy, err := NewProxy(ProxyConfig{Domain: testBrowserDomain, Routes: routes, DialContext: (&net.Dialer{}).DialContext})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, path, nil)
	request.Host = host
	recorder := httptest.NewRecorder()
	proxy.ServeHTTP(recorder, request)
	return recorder
}

func TestProxyHTTPSUpstreamSendsBrowserHostAsSNIWithoutVerifying(t *testing.T) {
	var sni string
	backend := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { // self-signed certificate
		sni = r.TLS.ServerName
		_, _ = w.Write([]byte("secure"))
	}))
	defer backend.Close()
	recorder := serveThrough(t, map[string]BrowserRoute{httpsTestHost: httpsTestRoute(t, backend, true)}, httpsTestHost, "/")
	if recorder.Code != http.StatusOK || recorder.Body.String() != "secure" {
		t.Fatalf("status=%d body=%q", recorder.Code, recorder.Body.String())
	}
	if sni != httpsTestHost {
		t.Fatalf("SNI=%q want %q", sni, httpsTestHost)
	}
}

func TestProxyHTTPRouteStaysPlainHTTP(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS != nil {
			t.Error("plain route spoke TLS")
		}
		_, _ = w.Write([]byte("plain"))
	}))
	defer backend.Close()
	host := "80.homelab." + testBrowserDomain
	if recorder := serveThrough(t, map[string]BrowserRoute{host: httpsTestRoute(t, backend, false)}, host, "/"); recorder.Body.String() != "plain" {
		t.Fatalf("body=%q", recorder.Body.String())
	}
}

func TestProxyRedirectToOwnAddressGetsExplanationNotALoop(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/elsewhere" {
			http.Redirect(w, r, "https://"+r.Host+"/login", http.StatusFound)
			return
		}
		http.Redirect(w, r, "https://"+r.Host+r.URL.RequestURI(), http.StatusPermanentRedirect) // Traefik's http->https entrypoint
	}))
	defer backend.Close()
	host := "80.homelab." + testBrowserDomain
	routes := map[string]BrowserRoute{host: httpsTestRoute(t, backend, false)}
	loop := serveThrough(t, routes, host, "/app?x=1")
	if loop.Code != http.StatusBadGateway || loop.Header().Get("X-Paperboat-Error") != "origin_redirect_loop" || loop.Header().Get("Location") != "" {
		t.Fatalf("loop: status=%d error=%q location=%q", loop.Code, loop.Header().Get("X-Paperboat-Error"), loop.Header().Get("Location"))
	}
	if other := serveThrough(t, routes, host, "/elsewhere"); other.Code != http.StatusFound {
		t.Fatalf("a redirect to another URL must pass through, got %d", other.Code)
	}
}

func TestBrowserRouteTableAcceptsFrontDeskSchemeDifferentFromPort(t *testing.T) {
	address := netip.MustParseAddr("100.64.0.9")
	numeric := "80.homelab." + testBrowserDomain
	wildcard := "*.homelab." + testBrowserDomain
	routes := map[string]BrowserRoute{numeric: {Address: address, Port: 80}, wildcard: {Address: address, Port: 80, HTTPS: true}}
	if _, _, _, err := browserRouteTable(routes, nil, testBrowserDomain); err != nil {
		t.Fatal(err)
	}
	routes[wildcard] = BrowserRoute{Address: address, Port: 81, HTTPS: true}
	if _, _, _, err := browserRouteTable(routes, nil, testBrowserDomain); err == nil {
		t.Fatal("a front desk to a different port must be refused")
	}
}
