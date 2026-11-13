package native

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/peertransport/peerquic"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/tailnet"
	"github.com/pinksaucepasta/paperboat/shared/serverclock"
)

type uptimePacketSocket struct {
	net.PacketConn
	remote net.Addr
}

func (p uptimePacketSocket) RemoteAddr() net.Addr { return p.remote }
func (uptimePacketSocket) MaxPayload() uint16     { return 1400 }

// A machine whose runtime has run longer than a QUIC leaf lives still accepts inbound: the listener serves the leaf
// issued at the handshake, not the one issued at startup (canary-b refused every inbound dial 23h45m after start,
// 2026-11-13; peers send no SNI and crypto/tls then served the startup leaf).
func TestRuntimeOlderThanTheLeafLifetimeStillAcceptsInbound(t *testing.T) {
	t.Cleanup(func() { serverclock.Observe(time.Now().UTC().Format(http.TimeFormat), time.Now(), time.Now()) })
	endpoint := func() (*tls.Config, tailnet.NetworkBinding) {
		public, private, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		issue := func() tls.Certificate {
			now := serverclock.Now()
			leaf := issueTestLeaf(t, private, now.Add(-15*time.Minute), now.Add(23*time.Hour+45*time.Minute), now.UnixNano())
			return tls.Certificate{Certificate: [][]byte{leaf.Raw}, PrivateKey: private}
		}
		// The production shape (runtime native_peer_identity.go): a startup leaf plus per-handshake issuers.
		config := &tls.Config{MinVersion: tls.VersionTLS13, MaxVersion: tls.VersionTLS13, InsecureSkipVerify: true, NextProtos: []string{peerquic.ALPN}, Certificates: []tls.Certificate{issue()},
			GetCertificate:       func(*tls.ClientHelloInfo) (*tls.Certificate, error) { c := issue(); return &c, nil },
			GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) { c := issue(); return &c, nil }} //nolint:gosec
		return config, tailnet.NetworkBinding{QUICPublicKey: base64.RawURLEncoding.EncodeToString(public)}
	}
	hostTLS, host := endpoint()
	dialerTLS, dialer := endpoint()
	// A day of uptime later on the server clock both peers judge leaves by.
	serverclock.Observe(time.Now().Add(24*time.Hour).UTC().Format(http.TimeFormat), time.Now(), time.Now())

	hostUDP, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dialerUDP, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer dialerUDP.Close()
	config := peerquic.DevelopmentSessionConfig(peerquic.ClassInteractive)
	listener, err := peerquic.ListenPacket(uptimePacketSocket{hostUDP, dialerUDP.LocalAddr()}, boundTLS(hostTLS, dialer, true), config)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	accepted := make(chan error, 1)
	go func() {
		s, err := listener.Accept(ctx)
		if err == nil {
			_ = s.Close()
		}
		accepted <- err
	}()
	session, err := peerquic.DialPacket(ctx, uptimePacketSocket{dialerUDP, hostUDP.LocalAddr()}, boundTLS(dialerTLS, host, false), config)
	if err != nil {
		t.Fatalf("inbound dial to a runtime a day old: %v", err)
	}
	defer session.Close() // open until the host has accepted: an early close can reach it before the handshake ends
	if err := <-accepted; err != nil {
		t.Fatalf("host accept: %v", err)
	}
}
