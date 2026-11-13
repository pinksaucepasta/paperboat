package datacarrier

import (
	"context"
	"crypto/tls"
	"net"
	"sync/atomic"
	"testing"
	"time"

	quic "github.com/quic-go/quic-go"
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
