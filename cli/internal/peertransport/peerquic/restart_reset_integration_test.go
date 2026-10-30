package peerquic_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"math/big"
	"net"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/peertransport/peerquic"
	"github.com/quic-go/quic-go"
)

type restartPacketSocket struct {
	net.PacketConn
	remote net.Addr
}

func (p *restartPacketSocket) RemoteAddr() net.Addr { return p.remote }
func (*restartPacketSocket) MaxPayload() uint16     { return 1400 }

func TestRestartedEndpointResetsThePreviousAuthenticatedQUICConnection(t *testing.T) {
	for _, sameIdentity := range []bool{true, false} {
		name := "same_identity"
		if !sameIdentity {
			name = "different_identity"
		}
		t.Run(name, func(t *testing.T) { testRestartedEndpointReset(t, sameIdentity) })
	}
}

func testRestartedEndpointReset(t *testing.T, sameIdentity bool) {
	clientTLS, serverTLS := probeTLSConfigs(t)
	clientUDP, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer clientUDP.Close()
	serverUDP, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := serverUDP.LocalAddr().String()
	config := peerquic.DevelopmentSessionConfig(peerquic.ClassInteractive)
	listener, err := peerquic.ListenPacket(&restartPacketSocket{serverUDP, clientUDP.LocalAddr()}, serverTLS, config)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	accepted := make(chan *peerquic.Session, 1)
	acceptErr := make(chan error, 1)
	go func() {
		s, err := listener.Accept(ctx)
		if err != nil {
			acceptErr <- err
			return
		}
		accepted <- s
	}()
	client, err := peerquic.DialPacket(ctx, &restartPacketSocket{clientUDP, serverUDP.LocalAddr()}, clientTLS, config)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	select {
	case <-accepted:
	case err := <-acceptErr:
		t.Fatal(err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	// Lose the endpoint process, including its in-memory connection IDs. Its
	// persisted TLS identity remains, as during a carrier binary replacement.
	_ = listener.Close()
	replacementUDP, err := net.ListenPacket("udp4", address)
	if err != nil {
		t.Fatal(err)
	}
	if sameIdentity {
		// Production reissues its TLS leaf at startup but keeps the endpoint key.
		previous := serverTLS.Certificates[0]
		template, err := x509.ParseCertificate(previous.Certificate[0])
		if err != nil {
			t.Fatal(err)
		}
		template.SerialNumber = big.NewInt(99)
		key := previous.PrivateKey.(ed25519.PrivateKey)
		leaf, err := x509.CreateCertificate(rand.Reader, template, template, key.Public(), key)
		if err != nil {
			t.Fatal(err)
		}
		serverTLS = serverTLS.Clone()
		serverTLS.Certificates = []tls.Certificate{{Certificate: [][]byte{leaf}, PrivateKey: key}}
	} else {
		_, serverTLS = probeTLSConfigs(t)
	}
	replacement, err := peerquic.ListenPacket(&restartPacketSocket{replacementUDP, clientUDP.LocalAddr()}, serverTLS, config)
	if err != nil {
		t.Fatal(err)
	}
	defer replacement.Close()
	stream, err := client.Connection.OpenStreamSync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Write(make([]byte, 128)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-client.Connection.Context().Done():
		if !sameIdentity {
			t.Fatal("another private identity reset an authenticated connection")
		}
		var reset *quic.StatelessResetError
		if !errors.As(context.Cause(client.Connection.Context()), &reset) {
			t.Fatal("previous connection did not receive an authenticated stateless reset")
		}
	case <-time.After(time.Second):
		if sameIdentity {
			t.Fatal("restarted endpoint left the old connection waiting for PTO/idle timeout")
		}
	}
}
