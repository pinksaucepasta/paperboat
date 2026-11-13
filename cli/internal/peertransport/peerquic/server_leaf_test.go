package peerquic_test

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/peertransport/peerquic"
)

// A listener serves the leaf its GetCertificate issues now, not the static one it started with: peers send no SNI,
// and crypto/tls then preferred the static leaf until it expired (one-way loss after 23h45m of uptime, 2026-11-13).
func TestListenerServesTheFreshLeafNotTheStartupOne(t *testing.T) {
	clientTLS, serverTLS := probeTLSConfigs(t)
	_, fresh := probeTLSConfigs(t)
	freshLeaf := fresh.Certificates[0]
	serverTLS.GetCertificate = func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return &freshLeaf, nil }
	clientTLS.VerifyConnection = func(state tls.ConnectionState) error {
		if len(state.PeerCertificates) != 1 || string(state.PeerCertificates[0].Raw) != string(freshLeaf.Certificate[0]) {
			return errors.New("server presented its startup leaf")
		}
		return nil
	}
	clientUDP, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer clientUDP.Close()
	serverUDP, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	config := peerquic.DevelopmentSessionConfig(peerquic.ClassInteractive)
	listener, err := peerquic.ListenPacket(&restartPacketSocket{serverUDP, clientUDP.LocalAddr()}, serverTLS, config)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	go func() {
		if s, err := listener.Accept(ctx); err == nil {
			defer s.Close()
			<-ctx.Done()
		}
	}()
	client, err := peerquic.DialPacket(ctx, &restartPacketSocket{clientUDP, serverUDP.LocalAddr()}, clientTLS, config)
	if err != nil {
		t.Fatal(err)
	}
	_ = client.Close()
}
