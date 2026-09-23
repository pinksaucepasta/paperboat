package splitdns

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestBrowserIsolationFixture exposes only ephemeral loopback listeners for an
// actual browser driver. Its CA belongs to this test and is never installed in a
// system trust store. The driver trusts only its SPKI in an isolated profile;
// native system trust has a separate acceptance test.
func TestBrowserIsolationFixture(t *testing.T) {
	output := os.Getenv("PAPERBOAT_BROWSER_FIXTURE")
	if output == "" {
		t.Skip("requires the browser isolation driver")
	}
	finished := make(chan struct{})
	var finish sync.Once
	app := func(name string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", "no-store")
			if origin := r.Header.Get("Origin"); origin != "" {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Set("Access-Control-Allow-Credentials", "true")
			}
			switch r.URL.Path {
			case "/finish":
				finish.Do(func() { close(finished) })
			case "/echo":
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]string{"cookie": r.Header.Get("Cookie"), "site": r.Header.Get("Sec-Fetch-Site")})
			default:
				http.SetCookie(w, &http.Cookie{Name: "header" + name, Value: name, Path: "/", Secure: true, SameSite: http.SameSiteStrictMode})
				http.SetCookie(w, &http.Cookie{Name: "badParent", Value: name, Domain: "pprbt", Path: "/", Secure: true})
				w.Header().Set("Content-Type", "text/html")
				_, _ = w.Write([]byte("<!doctype html><title>Paperboat browser isolation fixture</title>"))
			}
		}))
	}
	a, b := app("A"), app("B")
	defer a.Close()
	defer b.Close()
	routes := map[string]BrowserRoute{}
	hosts := make([]string, 0, 2)
	for _, server := range []*httptest.Server{a, b} {
		port := server.Listener.Addr().(*net.TCPAddr).Port
		host, err := BrowserHostname("browser-fixture-machine", port, "pprbt")
		if err != nil {
			t.Fatal(err)
		}
		hosts = append(hosts, host)
		routes[host] = BrowserRoute{Address: netip.MustParseAddr("127.0.0.1"), Port: port}
	}
	ca, err := LoadOrCreateConstrainedCA(t.TempDir(), "pprbt")
	if err != nil {
		t.Fatal(err)
	}
	proxy, err := NewProxy(ProxyConfig{Routes: routes, CA: ca, Suffix: "pprbt", DialContext: (&net.Dialer{}).DialContext})
	if err != nil {
		t.Fatal(err)
	}
	var pins []string
	for _, host := range hosts {
		cert, err := proxy.GetCertificate(&tls.ClientHelloInfo{ServerName: host})
		if err != nil {
			t.Fatal(err)
		}
		leaf, err := x509.ParseCertificate(cert.Certificate[0])
		if err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256(leaf.RawSubjectPublicKeyInfo)
		pins = append(pins, base64.StdEncoding.EncodeToString(digest[:]))
	}
	httpListener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer httpListener.Close()
	tlsListener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer tlsListener.Close()
	if err = proxy.StartListeners(httpListener, tlsListener); err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := proxy.Stop(ctx); err != nil {
			t.Error(err)
		}
	}()
	port := strconv.Itoa(tlsListener.Addr().(*net.TCPAddr).Port)
	data, _ := json.Marshal(map[string]any{"origins": []string{"https://" + hosts[0] + ":" + port, "https://" + hosts[1] + ":" + port}, "spki": strings.Join(pins, ",")})
	if err = os.WriteFile(output, data, 0600); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(output)
	select {
	case <-finished:
	case <-t.Context().Done():
		t.Fatal(t.Context().Err())
	}
}
