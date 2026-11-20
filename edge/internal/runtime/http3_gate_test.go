package runtime

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
)

func gatewayCertificate(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "gateway.example.test"}, DNSNames: []string{"gateway.example.test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// A visitor address whose QUIC handshakes never finish pins at most its share of the gateway; another address still gets
// its page, and a normal first request from a fresh address is served.
func TestHTTP3GatewayBoundsUnfinishedHandshakesPerAddress(t *testing.T) {
	serverTLS := &tls.Config{Certificates: []tls.Certificate{gatewayCertificate(t)}}
	server, err := NewHTTP3Server("127.0.0.1:0", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "ok") }), serverTLS, 4096)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	if err := server.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = server.Shutdown(context.Background()) }()
	target := server.packet.LocalAddr().(*net.UDPAddr)

	get := func(source string) (string, error) {
		packet, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP(source)})
		if err != nil {
			t.Fatal(err)
		}
		defer packet.Close()
		transport := &quic.Transport{Conn: packet}
		defer transport.Close()
		rt := &http3.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true, NextProtos: []string{http3.NextProtoH3}},
			Dial: func(ctx context.Context, _ string, tlsConf *tls.Config, conf *quic.Config) (*quic.Conn, error) {
				return transport.Dial(ctx, target, tlsConf, conf)
			}}
		defer rt.Close()
		reqCtx, reqCancel := context.WithTimeout(ctx, 5*time.Second)
		defer reqCancel()
		request, _ := http.NewRequestWithContext(reqCtx, http.MethodGet, "https://gateway.example.test/", nil)
		response, err := rt.RoundTrip(request)
		if err != nil {
			return "", err
		}
		defer response.Body.Close()
		body, _ := io.ReadAll(response.Body)
		return string(body), nil
	}
	if body, err := get("127.0.0.1"); err != nil || body != "ok" {
		t.Fatalf("first request from a fresh address: %q, %v", body, err)
	}

	// A client that stops at the server certificate leaves the server's handshake unfinished.
	var stalled atomic.Int32
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	stallTLS := &tls.Config{InsecureSkipVerify: true, NextProtos: []string{http3.NextProtoH3}, VerifyPeerCertificate: func([][]byte, [][]*x509.Certificate) error {
		stalled.Add(1)
		<-release
		return errors.New("test client stopped")
	}}
	for range visitorPendingHandshakes {
		go func() {
			packet, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
			if err != nil {
				return
			}
			defer packet.Close()
			_, _ = (&quic.Transport{Conn: packet}).Dial(ctx, target, stallTLS, &quic.Config{})
		}()
	}
	for deadline := time.Now().Add(10 * time.Second); stalled.Load() != visitorPendingHandshakes; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("%d handshakes reached the gateway, want %d", stalled.Load(), visitorPendingHandshakes)
		}
	}
	if body, err := get("127.0.0.1"); err == nil || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a handshake past one address's share was held or served: %q, %v", body, err)
	}
	if body, err := get("127.0.0.2"); err != nil || body != "ok" {
		t.Fatalf("another address: %q, %v", body, err)
	}
}

// HTTP/3 is never forwarded between nodes, so a node must not complete a QUIC handshake for a name another node holds:
// the browser's failed attempt falls back to TCP, where the visitor is forwarded. A name this node serves, and the
// infrastructure host, still get the normal response.
func TestHTTP3RefusesNamesTheNodeDoesNotHold(t *testing.T) {
	held := map[string]bool{"held.example.test": true, "gateway.example.test": true}
	server, err := NewHTTP3Server("127.0.0.1:0", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "ok "+r.Host) }),
		&tls.Config{Certificates: []tls.Certificate{gatewayCertificate(t)}}, 4096)
	if err != nil {
		t.Fatal(err)
	}
	server.Admit = func(host string) bool { return held[host] }
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	if err := server.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = server.Shutdown(context.Background()) }()
	target := server.packet.LocalAddr().(*net.UDPAddr)

	get := func(name string) (string, time.Duration, error) {
		packet, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
		if err != nil {
			t.Fatal(err)
		}
		defer packet.Close()
		transport := &quic.Transport{Conn: packet}
		defer transport.Close()
		rt := &http3.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true, NextProtos: []string{http3.NextProtoH3}},
			Dial: func(ctx context.Context, _ string, tlsConf *tls.Config, conf *quic.Config) (*quic.Conn, error) {
				return transport.Dial(ctx, target, tlsConf, conf)
			}}
		defer rt.Close()
		reqCtx, reqCancel := context.WithTimeout(ctx, 5*time.Second)
		defer reqCancel()
		request, _ := http.NewRequestWithContext(reqCtx, http.MethodGet, "https://"+name+"/", nil)
		started := time.Now()
		response, err := rt.RoundTrip(request)
		if err != nil {
			return "", time.Since(started), err
		}
		defer response.Body.Close()
		body, _ := io.ReadAll(response.Body)
		return string(body), time.Since(started), nil
	}
	for _, name := range []string{"held.example.test", "gateway.example.test"} {
		if body, _, err := get(name); err != nil || body != "ok "+name {
			t.Fatalf("%s over HTTP/3: %q, %v", name, body, err)
		}
	}
	for _, name := range []string{"other.example.test", "HELD.other.example.test"} {
		body, took, err := get(name)
		var transportErr *quic.TransportError
		if err == nil || errors.Is(err, context.DeadlineExceeded) || !errors.As(err, &transportErr) || !transportErr.ErrorCode.IsCryptoError() {
			t.Fatalf("%s: handshake for a name this node does not hold must fail with a TLS alert, got %q, %v", name, body, err)
		}
		if took > 2*time.Second {
			t.Fatalf("%s: refusal took %s, want it immediate", name, took)
		}
	}
}
