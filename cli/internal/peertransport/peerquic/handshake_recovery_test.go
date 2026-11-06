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

// The first dial after a peer's clean close gives up on a silent handshake in a fraction of the usual second, so the
// retry that follows a lost first flight is not delayed by the full idle timeout.
func TestHandshakeTimeoutBoundsASilentFirstFlight(t *testing.T) {
	clientTLS, _ := probeTLSConfigs(t)
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
	config.HandshakeTimeout = 250 * time.Millisecond
	started := time.Now()
	_, err = peerquic.DialPacket(t.Context(), &restartPacketSocket{clientUDP, serverUDP.LocalAddr()}, clientTLS, config)
	var idle *quic.IdleTimeoutError
	if !errors.As(err, &idle) || time.Since(started) > 700*time.Millisecond {
		t.Fatalf("the short handshake timeout was not honoured: type=%T elapsed=%s", err, time.Since(started))
	}
	config.HandshakeTimeout = -time.Second
	if _, err := peerquic.DialPacket(t.Context(), &restartPacketSocket{clientUDP, serverUDP.LocalAddr()}, clientTLS, config); err == nil {
		t.Fatal("a negative handshake timeout was accepted")
	}
}
