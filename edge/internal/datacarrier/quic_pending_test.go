package datacarrier

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
)

// One address whose QUIC handshakes never finish pins at most its share; another address still connects,
// and finished handshakes give their slots back (more connections than the share succeed in turn).
func TestQUICListenerBoundsUnfinishedHandshakesPerAddress(t *testing.T) {
	clientTLS, serverTLS, _ := testEndpointCertificates(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	config := testCarrierConfig(4)
	listener, err := ListenQUIC(ctx, EndpointConfig{Address: "127.0.0.1:0", TLS: serverTLS, PeerBinding: func(tls.ConnectionState) (Identity, error) {
		return testCarrierIdentity(), nil
	}}, config)
	if err != nil {
		t.Fatalf("listen QUIC: %v", err)
	}
	defer listener.Close()
	dial := func(ctx context.Context, source string, tlsConf *tls.Config) (*quic.Conn, error) {
		packet, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP(source)})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = packet.Close() })
		return quic.Dial(ctx, packet, listener.Addr(), tlsConf, endpointQUICConfig(config))
	}

	// A client that stops at its certificate leaves the server's handshake unfinished.
	var stalled atomic.Int32
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	stallTLS := clientTLS.Clone()
	stallTLS.GetClientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
		stalled.Add(1)
		<-release
		return nil, errors.New("test client stopped")
	}
	for range pendingHandshakesPerAddress {
		go func() { _, _ = dial(ctx, "127.0.0.1", stallTLS) }()
	}
	for deadline := time.Now().Add(10 * time.Second); stalled.Load() != pendingHandshakesPerAddress; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("%d handshakes reached the server, want %d", stalled.Load(), pendingHandshakesPerAddress)
		}
	}
	overCtx, overCancel := context.WithTimeout(ctx, 5*time.Second)
	defer overCancel()
	if _, err := dial(overCtx, "127.0.0.1", clientTLS.Clone()); err == nil || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a handshake past one address's share was held or served: %v", err)
	}

	for range pendingHandshakesPerAddress + 1 {
		accepted := make(chan error, 1)
		go func() {
			server, err := listener.Accept(ctx)
			if err == nil {
				t.Cleanup(func() { _ = server.Close() })
			}
			accepted <- err
		}()
		connection, err := dial(ctx, "127.0.0.2", clientTLS.Clone())
		if err != nil {
			t.Fatalf("second address dial: %v", err)
		}
		client, err := NewClientWithSession(ctx, newQUICSession(connection), config)
		if err != nil {
			t.Fatalf("second address carrier: %v", err)
		}
		t.Cleanup(func() { _ = client.Close() })
		if err := <-accepted; err != nil {
			t.Fatalf("second address accept: %v", err)
		}
	}
}
