package peerquic_test

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/peertransport/peerquic"
	"github.com/quic-go/quic-go"
)

func TestSilentHandshakeReturnsBeforePTOBackoffStrandsRecovery(t *testing.T) {
	clientTLS, serverTLS := probeTLSConfigs(t)
	serverUDP, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer serverUDP.Close()
	clientUDP, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	config := peerquic.DevelopmentSessionConfig(peerquic.ClassInteractive)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	started := time.Now()
	_, err = peerquic.DialPacket(ctx, &restartPacketSocket{clientUDP, serverUDP.LocalAddr()}, clientTLS, config)
	var idle *quic.IdleTimeoutError
	if !errors.As(err, &idle) || time.Since(started) > 1800*time.Millisecond {
		t.Fatalf("silent handshake did not expire promptly: type=%T elapsed=%s", err, time.Since(started))
	}
	// The restarted carrier receives a fresh Initial rather than waiting for the
	// abandoned connection's exponentially delayed retransmission.
	freshUDP, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer freshUDP.Close()
	listener, err := peerquic.ListenPacket(&restartPacketSocket{serverUDP, freshUDP.LocalAddr()}, serverTLS, config)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	freshCtx, stopFresh := context.WithTimeout(t.Context(), 5*time.Second)
	defer stopFresh()
	established, err := peerquic.DialPacket(freshCtx, &restartPacketSocket{freshUDP, serverUDP.LocalAddr()}, clientTLS, config)
	if err != nil {
		t.Fatal(err)
	}
	defer established.Close()
	accepted, err := listener.Accept(freshCtx)
	if err != nil {
		t.Fatal(err)
	}
	defer accepted.Close()
	// Handshake recovery must not shorten the established connection's idle bound.
	time.Sleep(1200 * time.Millisecond)
	if err := established.Connection.Context().Err(); err != nil {
		t.Fatalf("established connection expired: %v", err)
	}
}
