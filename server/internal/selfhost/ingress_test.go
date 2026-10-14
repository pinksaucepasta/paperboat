package selfhost

import (
	"context"
	"crypto/x509"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"
)

func TestPublicIngressAddressBoundary(t *testing.T) {
	for _, text := range []string{"127.0.0.1", "10.1.2.3", "100.64.0.1", "169.254.169.254", "192.0.2.1", "198.18.0.1", "224.0.0.1", "240.0.0.1", "::1", "fc00::1", "fe80::1", "::ffff:127.0.0.1", "2001:db8::1"} {
		if publicIngressIP(netip.MustParseAddr(text)) {
			t.Fatalf("non-public probe address accepted: %s", text)
		}
	}
	for _, text := range []string{"8.8.8.8", "2606:4700:4700::1111"} {
		if !publicIngressIP(netip.MustParseAddr(text)) {
			t.Fatalf("public address rejected: %s", text)
		}
	}
}

func TestPublicIngressRejectsSelfSignedHTTPSBeforeNodeProof(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1); w.WriteHeader(http.StatusOK) }))
	t.Cleanup(server.Close)
	transport := publicIngressTransport("example.com", netip.MustParseAddr("127.0.0.1"))
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
	}
	t.Cleanup(transport.CloseIdleConnections)
	client := &http.Client{Transport: transport, Timeout: time.Second}
	_, err := client.Get("https://example.com/.well-known/paperboat-installation")
	var untrusted x509.UnknownAuthorityError
	if !errors.As(err, &untrusted) {
		t.Fatalf("untrusted HTTPS certificate denial = %T %v", err, err)
	}
	if requests.Load() != 0 {
		t.Fatal("public ingress reached node proof over untrusted HTTPS")
	}
}
