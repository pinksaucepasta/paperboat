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
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	config := testCarrierConfig(4)
	config.KeepAliveInterval = DefaultConfig().KeepAliveInterval
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
	select {
	case <-carrier.Done():
		t.Fatal("live idle carrier closed")
	case <-time.After(16 * time.Second):
	}
	dropped.Store(true)
	started := time.Now()
	select {
	case <-carrier.Done():
		t.Logf("edge detected blackhole after %v", time.Since(started))
	case <-time.After(18 * time.Second):
		t.Fatal("edge kept a blackholed carrier beyond the 15-second idle limit")
	}
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
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	config := ServiceConfig{Carrier: testCarrierConfig(2)}
	config.Carrier.KeepAliveInterval = DefaultConfig().KeepAliveInterval
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
	select {
	case <-carrier.Done():
		t.Fatal("live idle HTTP/3 carrier closed")
	case <-time.After(16 * time.Second):
	}
	dropped.Store(true)
	started := time.Now()
	select {
	case <-carrier.Done():
		t.Logf("edge detected blackholed HTTP/3 carrier after %v", time.Since(started))
	case <-time.After(18 * time.Second):
		t.Fatal("edge kept a blackholed HTTP/3 carrier beyond the 15-second idle limit")
	}
}
