package splitdns

import (
	"context"
	"crypto/tls"
	"github.com/pinksaucepasta/paperboat/internal/testcert"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
)

func TestMachineProxyPreservesHostAndExactRoutes(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, r.Host) }))
	defer origin.Close()
	ca, err := testcert.New()
	if err != nil {
		t.Fatal(err)
	}
	base := "homelab." + BrowserSuffix
	fallback := BrowserRoute{Address: netip.MustParseAddr("127.0.0.1"), Port: 80}
	exact := BrowserRoute{Address: fallback.Address, Port: 8080}
	if _, err := NewProxy(ProxyConfig{Routes: map[string]BrowserRoute{"*." + base: fallback}, DialContext: (&net.Dialer{}).DialContext}); err == nil {
		t.Fatal("proxy introduced an unregistered port")
	}
	routes := map[string]BrowserRoute{"*." + base: fallback, "80." + base: fallback, "8080." + base: exact, "fixed." + base: exact}
	p, err := NewProxy(ProxyConfig{Routes: routes, DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", origin.Listener.Addr().String())
	}, IssueCertificate: func(_ context.Context, name string) (tls.Certificate, error) { return ca.TLS(name) }})
	if err != nil {
		t.Fatal(err)
	}
	defer p.transport.CloseIdleConnections()
	for _, app := range []string{"first", "second", "third"} {
		name := app + "." + base
		r := httptest.NewRequest("GET", "https://"+name+"/", nil)
		r.TLS = &tls.ConnectionState{ServerName: name}
		w := httptest.NewRecorder()
		p.ServeHTTP(w, r)
		if w.Code != 200 || w.Body.String() != name {
			t.Fatalf("Host routing: %d %s", w.Code, w.Body.String())
		}
		if _, err := p.GetCertificate(&tls.ClientHelloInfo{ServerName: name}); err != nil {
			t.Fatal(err)
		}
	}
	if len(p.routes) != 4 {
		t.Fatal("per-app route state")
	}
	for _, host := range []string{"8080." + base, "fixed." + base} {
		r, ok := ResolveBrowserRoute(p.routes, host, BrowserSuffix)
		if !ok || r.Port != 8080 {
			t.Fatal("exact priority")
		}
	}
	for _, name := range []string{"81." + base, "080." + base, "0." + base, "65536." + base, "foo.bar." + base, "app.other." + BrowserSuffix} {
		w := httptest.NewRecorder()
		p.ServeHTTP(w, httptest.NewRequest("GET", "http://"+name+"/", nil))
		if w.Code != 421 {
			t.Fatalf("forwarded %s", name)
		}
		if _, err := p.GetCertificate(&tls.ClientHelloInfo{ServerName: name}); err == nil {
			t.Fatalf("certificate for %s", name)
		}
	}
	r := httptest.NewRequest("GET", "https://first."+base+"/", nil)
	r.TLS = &tls.ConnectionState{ServerName: "second." + base}
	w := httptest.NewRecorder()
	p.ServeHTTP(w, r)
	if w.Code != 421 {
		t.Fatal("SNI mismatch admitted")
	}
	w = httptest.NewRecorder()
	p.ServeHTTP(w, httptest.NewRequest("GET", "http://"+base+"/", nil))
	if w.Code != 200 {
		t.Fatal("machine index removed")
	}
}
