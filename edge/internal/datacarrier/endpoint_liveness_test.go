package datacarrier

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	quic "github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
)

// The edge must detect a silent path even when its peer advertises a long idle timeout.
func TestEdgeQUICCarrierDetectsABlackholedPath(t *testing.T) {
	clientTLS, serverTLS, _ := testEndpointCertificates(t)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	config := testCarrierConfig(4)
	config.KeepAliveInterval = DefaultConfig().KeepAliveInterval
	config.QUICIdleTimeout = livenessIdleTimeout
	listener, err := ListenQUIC(ctx, EndpointConfig{Address: "127.0.0.1:0", TLS: serverTLS,
		PeerBinding: func(tls.ConnectionState) (Identity, error) { return config.Identity, nil },
	}, config)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	dropped, address := endpointUDPBlackhole(t, listener.Addr().(*net.UDPAddr))
	client, err := quic.DialAddr(ctx, address, clientTLS, &quic.Config{MaxIdleTimeout: 2 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseWithError(0, "test finished")
	carrier, err := listener.Accept(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer carrier.Close()
	// With no application traffic or peer keepalives, edge keepalives preserve a live path.
	requireAliveBeyondIdle(t, carrier.Done())
	dropped.Store(true)
	requireDetectsBlackhole(t, carrier.Done())
}

func endpointUDPBlackhole(t *testing.T, target *net.UDPAddr) (*atomic.Bool, string) {
	t.Helper()
	front, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = front.Close() })
	back, err := net.DialUDP("udp", nil, target)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = back.Close() })
	dropped := &atomic.Bool{}
	var client atomic.Pointer[net.UDPAddr]
	go func() {
		buffer := make([]byte, 64<<10)
		for {
			n, from, err := front.ReadFromUDP(buffer)
			if err != nil {
				return
			}
			client.Store(from)
			if !dropped.Load() {
				_, _ = back.Write(buffer[:n])
			}
		}
	}()
	go func() {
		buffer := make([]byte, 64<<10)
		for {
			n, err := back.Read(buffer)
			if err != nil {
				return
			}
			if to := client.Load(); to != nil && !dropped.Load() {
				_, _ = front.WriteToUDP(buffer[:n], to)
			}
		}
	}()
	return dropped, front.LocalAddr().String()
}

// The production carrier is HTTP/3 CONNECT; its edge side must also find a silent path within 15 s
// when the machine advertises a long idle timeout and sends no keepalives of its own.
func TestHTTPServiceHTTP3CarrierDetectsABlackholedPath(t *testing.T) {
	clientTLS, serverTLS, _ := testEndpointCertificates(t)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	config := ServiceConfig{Carrier: testCarrierConfig(2)}
	config.Carrier.KeepAliveInterval = DefaultConfig().KeepAliveInterval
	config.Carrier.QUICIdleTimeout = livenessIdleTimeout
	config.QUIC = &EndpointConfig{Address: "127.0.0.1:0", TLS: serverTLS,
		PeerBinding: func(tls.ConnectionState) (Identity, error) { return config.Carrier.Identity, nil }}
	service, err := NewHTTPService(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	dropped, address := endpointUDPBlackhole(t, service.QUICAddr().(*net.UDPAddr))
	clientTLS.NextProtos = []string{"h3"}
	transport := &http3.Transport{TLSClientConfig: clientTLS, QUICConfig: &quic.Config{MaxIdleTimeout: 2 * time.Minute}}
	defer transport.Close()
	bodyReader, bodyWriter := io.Pipe()
	defer bodyWriter.Close()
	request, err := http.NewRequestWithContext(ctx, http.MethodConnect, "https://"+address+httpCarrierPath, bodyReader)
	if err != nil {
		t.Fatal(err)
	}
	response, err := transport.RoundTrip(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	carrier, err := service.Accept(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer carrier.Close()
	requireAliveBeyondIdle(t, carrier.Done())
	dropped.Store(true)
	requireDetectsBlackhole(t, carrier.Done())
}

// The default configuration is the production one: 10 s idle with a keepalive at half of it (idle plus keepalive stays
// within 15 s); an injected idle timeout moves both, and a shorter keepalive interval still wins.
func TestEndpointQUICConfigDefaultsAreTheProductionLimits(t *testing.T) {
	for _, tc := range []struct {
		name             string
		keepAlive, idle  time.Duration
		wantIdle, wantKA time.Duration
	}{
		{"zero idle is production", DefaultConfig().KeepAliveInterval, 0, 10 * time.Second, 5 * time.Second},
		{"a short interval stays", time.Second, 0, 10 * time.Second, time.Second},
		{"injected idle halves the keepalive", DefaultConfig().KeepAliveInterval, 4 * time.Second, 4 * time.Second, 2 * time.Second},
	} {
		got := endpointQUICConfig(Config{KeepAliveInterval: tc.keepAlive, QUICIdleTimeout: tc.idle})
		if got.MaxIdleTimeout != tc.wantIdle || got.KeepAlivePeriod != tc.wantKA {
			t.Fatalf("%s: idle %v keepalive %v, want %v and %v", tc.name, got.MaxIdleTimeout, got.KeepAlivePeriod, tc.wantIdle, tc.wantKA)
		}
	}
	if config := DefaultConfig(); config.QUICIdleTimeout != 0 || config.withDefaults().QUICIdleTimeout != 0 {
		t.Fatal("the production configuration must leave the idle timeout at its zero default")
	}
	bad := testCarrierConfig(2)
	bad.QUICIdleTimeout = -time.Second
	if bad.validateListener() == nil {
		t.Fatal("a negative idle timeout was accepted")
	}
}

// livenessIdleTimeout stands for the production 10 s idle timeout, so the tests of the 15 s liveness limit run in seconds.
// The edge's own idle (and its keepalive at half of it) is far below the 2 minutes its peer advertises.
const livenessIdleTimeout = 4 * time.Second

// requireAliveBeyondIdle: a live idle path carries only keepalives, and they must hold it past twice the idle timeout.
func requireAliveBeyondIdle(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
		t.Fatalf("a live idle carrier closed within %v", 2*livenessIdleTimeout)
	case <-time.After(2 * livenessIdleTimeout):
	}
}

// requireDetectsBlackhole waits for the edge to notice a silent path. The bound only ends a hang: it is below the 2
// minutes the peer advertises, so only the edge's own idle timeout can have closed the carrier within it.
func requireDetectsBlackhole(t *testing.T, done <-chan struct{}) {
	t.Helper()
	started := time.Now()
	select {
	case <-done:
		t.Logf("edge detected the blackholed carrier after %v", time.Since(started))
	case <-time.After(time.Minute):
		t.Fatal("edge kept a blackholed carrier although its own idle timeout is far below the peer's 2 minutes")
	}
}
