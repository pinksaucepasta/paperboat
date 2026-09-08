package native

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"net"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/peertransport/peerquic"
	"github.com/quic-go/quic-go"
)

func TestReconnectRequiredClassification(t *testing.T) {
	peerID := "peer_01"
	tests := []struct {
		name         string
		err          error
		localClosing bool
		want         bool
	}{
		{name: "idle timeout", err: &quic.IdleTimeoutError{}, want: true},
		{name: "stateless reset", err: &quic.StatelessResetError{}, want: true},
		{name: "unexpected transport close", err: quic.ErrTransportClosed, want: true},
		{name: "remote graceful close", err: &quic.ApplicationError{Remote: true}, want: true},
		{name: "caller cancellation", err: context.Canceled},
		{name: "caller deadline", err: context.DeadlineExceeded},
		{name: "stream reset", err: &quic.StreamError{StreamID: 4, ErrorCode: 7, Remote: true}},
		{name: "local close", err: quic.ErrTransportClosed, localClosing: true},
		{name: "remote application failure", err: &quic.ApplicationError{Remote: true, ErrorCode: 8}},
		{name: "protocol violation", err: &quic.TransportError{Remote: true, ErrorCode: quic.ProtocolViolation}},
		{name: "crypto violation", err: &quic.TransportError{Remote: true, ErrorCode: 0x100 + 42}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := reconnectRequired(peerID, test.err, test.localClosing)
			var reconnect *ReconnectRequiredError
			if errors.As(got, &reconnect) != test.want {
				t.Fatalf("error=%T %v, reconnect=%t", got, got, !test.want)
			}
			if !errors.Is(got, test.err) {
				t.Fatalf("underlying cause was not preserved: %v", got)
			}
			if test.want && reconnect.PeerID != peerID {
				t.Fatalf("peer ID=%q, want %q", reconnect.PeerID, peerID)
			}
		})
	}
}

func TestEstablishedPeerCloseRequiresReconnectOnOpen(t *testing.T) {
	clientTLS, serverTLS := recoveryTLS(t)
	clientPacket, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	serverPacket, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		_ = clientPacket.Close()
		t.Fatal(err)
	}
	clientSocket := &recoveryPacketSocket{PacketConn: clientPacket, remote: serverPacket.LocalAddr()}
	serverSocket := &recoveryPacketSocket{PacketConn: serverPacket, remote: clientPacket.LocalAddr()}
	config := peerquic.DevelopmentSessionConfig(peerquic.ClassInteractive)
	listener, err := peerquic.ListenPacket(serverSocket, serverTLS, config)
	if err != nil {
		_ = clientSocket.Close()
		t.Fatal(err)
	}
	defer listener.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	type acceptResult struct {
		session *peerquic.Session
		err     error
	}
	accepted := make(chan acceptResult, 1)
	go func() {
		session, acceptErr := listener.Accept(ctx)
		accepted <- acceptResult{session: session, err: acceptErr}
	}()
	clientQUIC, err := peerquic.DialPacket(ctx, clientSocket, clientTLS, config)
	if err != nil {
		t.Fatal(err)
	}
	defer clientQUIC.Close()
	serverResult := <-accepted
	if serverResult.err != nil {
		t.Fatal(serverResult.err)
	}
	defer serverResult.session.Close()
	if err := serverResult.session.Connection.CloseWithError(0, ""); err != nil {
		t.Fatal(err)
	}
	select {
	case <-clientQUIC.Connection.Context().Done():
	case <-ctx.Done():
		t.Fatalf("client did not observe peer close: %v", ctx.Err())
	}

	session := newSession(nil, "peer_remote", clientQUIC)
	_, err = session.Open(ctx)
	var reconnect *ReconnectRequiredError
	if !errors.As(err, &reconnect) || reconnect.PeerID != "peer_remote" {
		t.Fatalf("open error=%T %v", err, err)
	}
}

type recoveryPacketSocket struct {
	net.PacketConn
	remote net.Addr
}

func (s *recoveryPacketSocket) RemoteAddr() net.Addr { return s.remote }
func (*recoveryPacketSocket) MaxPayload() uint16     { return 1400 }

func recoveryTLS(t *testing.T) (*tls.Config, *tls.Config) {
	t.Helper()
	certificate := func(name string) tls.Certificate {
		public, private, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		template := &x509.Certificate{
			SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: name},
			NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
			KeyUsage:    x509.KeyUsageDigitalSignature,
			ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
		}
		raw, err := x509.CreateCertificate(rand.Reader, template, template, public, private)
		if err != nil {
			t.Fatal(err)
		}
		return tls.Certificate{Certificate: [][]byte{raw}, PrivateKey: private}
	}
	verify := func(tls.ConnectionState) error { return nil }
	common := tls.Config{MinVersion: tls.VersionTLS13, InsecureSkipVerify: true, VerifyConnection: verify, NextProtos: []string{peerquic.ALPN}}
	client, server := common.Clone(), common.Clone()
	client.Certificates = []tls.Certificate{certificate("client")}
	server.Certificates = []tls.Certificate{certificate("server")}
	server.ClientAuth = tls.RequireAnyClientCert
	return client, server
}
