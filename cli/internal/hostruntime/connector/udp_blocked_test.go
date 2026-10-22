package connector

import (
	"context"
	"errors"
	"net"
	"os"
	"sync"
	"syscall"
	"testing"
	"time"
)

// udpRefused is what quic-go returns when the first datagram cannot leave the
// machine: a local firewall's OUTPUT drop (EPERM, as on laner-b with Lane O's
// nft rules), or no route for UDP (ENETUNREACH, EHOSTUNREACH).
func udpRefused(errno syscall.Errno) error {
	send := &net.OpError{Op: "write", Net: "udp", Err: os.NewSyscallError("sendmsg", errno)}
	return &HTTPCarrierFailure{Stage: "round_trip", Cause: errors.Join(ErrDataCarrierTLS, send)}
}

func TestHTTP3FailureFallsBackOnImmediateUDPRefusal(t *testing.T) {
	ctx := context.Background()
	for _, errno := range []syscall.Errno{syscall.EPERM, syscall.EACCES, syscall.ENETUNREACH, syscall.EHOSTUNREACH} {
		if !http3FailureFallsBack(ctx, udpRefused(errno)) {
			t.Errorf("HTTP/3 refused with %v does not fall back to HTTP/2", errno)
		}
	}
	for name, err := range map[string]error{
		"invalid endpoint":  ErrInvalidDataCarrierEndpoint,
		"identity rejected": &dataCarrierPeerAuthorityFailure{cause: ErrDataCarrierAdmission},
	} {
		if http3FailureFallsBack(ctx, err) {
			t.Errorf("%s falls back although HTTP/2 meets the same refusal", name)
		}
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if http3FailureFallsBack(canceled, udpRefused(syscall.EPERM)) {
		t.Error("a canceled operation started another attempt")
	}
}

// With UDP refused, the pool reaches the edge over HTTP/2 at once, later
// connections start with HTTP/2, and QUIC is tried first again once the
// preference expires.
func TestDataCarrierPoolPrefersHTTP2WhileUDPIsRefused(t *testing.T) {
	config := testDataCarrierConfig()
	var (
		mu         sync.Mutex
		calls      []Transport
		udpBlocked = true
	)
	dialer := func(ctx context.Context, request DataCarrierDialRequest) (DataCarrierDialResult, error) {
		mu.Lock()
		calls = append(calls, request.Transport)
		blocked := udpBlocked
		mu.Unlock()
		if request.Transport == HTTP3 && blocked {
			return DataCarrierDialResult{}, &TransportDialError{Transport: HTTP3, Err: udpRefused(syscall.EPERM), Fallback: http3FailureFallsBack(ctx, udpRefused(syscall.EPERM))}
		}
		local, remote := net.Pipe()
		if _, err := NewDataCarrierServer(context.Background(), remote, config, DataCarrierAdmission{Identity: testDataCarrierIdentity(), Authorize: func(context.Context, StreamOpen) error { return nil }}); err != nil {
			return DataCarrierDialResult{}, err
		}
		return DataCarrierDialResult{Link: local, PeerIdentity: request.Identity, Transport: request.Transport, EdgeID: request.EdgeID, FailureDomain: request.FailureDomain}, nil
	}
	poolConfig := DataCarrierPoolConfig{MaximumCarriers: 1, QueueDepth: 2, Preferred: HTTP3, Fallback: HTTP2, EdgeID: "edge-a", FailureDomains: []string{"edge-a"}, Session: testDataCarrierIdentity(), Carrier: config}
	pool, err := NewDataCarrierPool(context.Background(), poolConfig, dialer)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	reconnect := func() Transport {
		t.Helper()
		if _, _, _, err := pool.dialWithFallback(context.Background(), 0); err != nil {
			t.Fatalf("dial with UDP refused: %v", err)
		}
		mu.Lock()
		defer mu.Unlock()
		first := calls[0]
		calls = nil
		return first
	}
	if err := pool.Connect(context.Background()); err != nil {
		t.Fatalf("UDP-refused network got no carrier: %v", err)
	}
	if got, _ := pool.SelectedTransport(); got != HTTP2 {
		t.Fatalf("selected %s, want %s", got, HTTP2)
	}
	mu.Lock()
	calls = nil
	mu.Unlock()
	if first := reconnect(); first != HTTP2 {
		t.Fatalf("reconnect while UDP is refused tried %s first", first)
	}
	mu.Lock()
	udpBlocked = false
	mu.Unlock()
	pool.fallbackUntil = time.Now().Add(-time.Second)
	if first := reconnect(); first != HTTP3 {
		t.Fatalf("after the preference expired the pool tried %s first, want QUIC again", first)
	}
}
