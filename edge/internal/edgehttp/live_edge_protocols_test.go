package edgehttp

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	"golang.org/x/net/http2"
)

// TestLiveTunnelEdgeProtocols qualifies a deployed public edge (PLAN S1-09). It
// needs a published address whose origin answers GET / with a success status:
//
//	PAPERBOAT_LIVE_EDGE_URL=https://<generated-address>.<public base domain>/ \
//	PAPERBOAT_LIVE_EDGE_WEBSOCKET_PATH=/ws   (optional: an origin path that accepts WebSocket)
//	PAPERBOAT_LIVE_EDGE_IP=192.0.2.1         (optional: dial this edge instead of the DNS answer, one run per edge)
//	go test ./internal/edgehttp -run TestLiveTunnelEdgeProtocols -v
//
// It prints one result per protocol; the run is the evidence recorded in CHECKLIST.md.
func TestLiveTunnelEdgeProtocols(t *testing.T) {
	address := strings.TrimSpace(os.Getenv("PAPERBOAT_LIVE_EDGE_URL"))
	if address == "" {
		t.Skip("set PAPERBOAT_LIVE_EDGE_URL to a live published https address to qualify the edge")
	}
	target, err := url.Parse(address)
	if err != nil || target.Scheme != "https" || target.Hostname() == "" {
		t.Fatalf("PAPERBOAT_LIVE_EDGE_URL must be an https address: %q", address)
	}
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	pin := strings.TrimSpace(os.Getenv("PAPERBOAT_LIVE_EDGE_IP"))
	// dialAddress keeps the port and swaps the host for the pinned edge; TLS still verifies the published name.
	dialAddress := func(address string) string {
		if pin == "" {
			return address
		}
		_, port, _ := net.SplitHostPort(address)
		return net.JoinHostPort(pin, port)
	}
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	dialTCP := func(ctx context.Context, network, address string) (net.Conn, error) {
		return dialer.DialContext(ctx, network, dialAddress(address))
	}
	h1 := func() *http.Transport {
		return &http.Transport{TLSClientConfig: tlsConfig, DialContext: dialTCP,
			TLSNextProto: map[string]func(string, *tls.Conn) http.RoundTripper{}}
	}
	get := func(t *testing.T, transport http.RoundTripper, wantProto string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
		if err != nil {
			t.Fatal(err)
		}
		response, err := (&http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(request)
		if err != nil {
			t.Fatalf("%s request failed: %v", wantProto, err)
		}
		defer response.Body.Close()
		if response.Proto != wantProto || response.StatusCode < 200 || response.StatusCode >= 400 {
			t.Fatalf("%s: negotiated %s with status %d", wantProto, response.Proto, response.StatusCode)
		}
		t.Logf("%s: %d from %s (edge %q)", wantProto, response.StatusCode, target.Host, pin)
	}
	t.Run("HTTP/1.1", func(t *testing.T) {
		get(t, h1(), "HTTP/1.1")
	})
	t.Run("HTTP/2", func(t *testing.T) {
		get(t, &http2.Transport{TLSClientConfig: tlsConfig, DialTLSContext: func(ctx context.Context, network, address string, config *tls.Config) (net.Conn, error) {
			return (&tls.Dialer{NetDialer: dialer, Config: config}).DialContext(ctx, network, dialAddress(address))
		}}, "HTTP/2.0")
	})
	t.Run("HTTP/3", func(t *testing.T) {
		transport := &http3.Transport{TLSClientConfig: tlsConfig, Dial: func(ctx context.Context, address string, config *tls.Config, quicConfig *quic.Config) (*quic.Conn, error) {
			return quic.DialAddr(ctx, dialAddress(address), config, quicConfig)
		}}
		defer transport.Close()
		get(t, transport, "HTTP/3.0")
	})
	t.Run("WebSocket", func(t *testing.T) {
		path := strings.TrimSpace(os.Getenv("PAPERBOAT_LIVE_EDGE_WEBSOCKET_PATH"))
		if path == "" {
			t.Skip("set PAPERBOAT_LIVE_EDGE_WEBSOCKET_PATH to an origin path that accepts WebSocket")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		endpoint := *target
		endpoint.Path = path
		connection, response, err := websocket.Dial(ctx, endpoint.String(), &websocket.DialOptions{HTTPClient: &http.Client{Transport: h1()}})
		if err != nil {
			t.Fatalf("WebSocket upgrade failed: %v", err)
		}
		defer connection.CloseNow()
		if response.StatusCode != http.StatusSwitchingProtocols {
			t.Fatalf("WebSocket upgrade status %d", response.StatusCode)
		}
		t.Logf("WebSocket: upgraded on %s (edge %q)", target.Host, pin)
	})
}
