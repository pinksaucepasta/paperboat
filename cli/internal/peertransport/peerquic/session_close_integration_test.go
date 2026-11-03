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

func TestOwnedTransportCloseInterruptsBlockedQUICShutdown(t *testing.T) {
	clientTLS, serverTLS := probeTLSConfigs(t)
	clientConn, serverConn := net.Pipe()
	listener, err := peerquic.Listen(serverConn, serverTLS, peerquic.ClassPreview)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	accepted := make(chan *peerquic.Session, 1)
	acceptErrors := make(chan error, 1)
	go func() {
		session, acceptErr := listener.Accept(ctx)
		if acceptErr != nil {
			acceptErrors <- acceptErr
			return
		}
		accepted <- session
	}()
	client, err := peerquic.Dial(ctx, clientConn, clientTLS, peerquic.ClassPreview)
	if err != nil {
		_ = listener.Close()
		t.Fatal(err)
	}
	var server *peerquic.Session
	select {
	case server = <-accepted:
	case acceptErr := <-acceptErrors:
		_ = client.Close()
		_ = listener.Close()
		t.Fatal(acceptErr)
	case <-ctx.Done():
		_ = client.Close()
		_ = listener.Close()
		t.Fatal(ctx.Err())
	}

	closed := make(chan error, 1)
	go func() { closed <- client.Close() }()
	select {
	case <-closed:
	case <-time.After(time.Second):
		_ = listener.Close()
		t.Fatal("owned QUIC transport close blocked")
	}
	go func() { closed <- server.Close() }()
	select {
	case <-closed:
	case <-time.After(time.Second):
		_ = listener.Close()
		t.Fatal("accepted QUIC session close blocked after peer transport shutdown")
	}
	_ = listener.Close()
}

func TestListenerShutdownDeliversRemoteApplicationClose(t *testing.T) {
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
	defer serverUDP.Close()
	config := peerquic.DevelopmentSessionConfig(peerquic.ClassInteractive)
	listener, err := peerquic.ListenPacket(&restartPacketSocket{serverUDP, clientUDP.LocalAddr()}, serverTLS, config)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	accepted := make(chan error, 1)
	go func() { _, err := listener.Accept(ctx); accepted <- err }()
	client, err := peerquic.DialPacket(ctx, &restartPacketSocket{clientUDP, serverUDP.LocalAddr()}, clientTLS, config)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if err := <-accepted; err != nil {
		t.Fatal(err)
	}
	closed := make(chan error, 1)
	go func() { closed <- listener.Close() }()
	select {
	case <-client.Connection.Context().Done():
		var remote *quic.ApplicationError
		if !errors.As(context.Cause(client.Connection.Context()), &remote) || !remote.Remote {
			t.Fatal("listener shutdown did not deliver an authenticated remote application close")
		}
	case <-time.After(time.Second):
		t.Fatal("listener shutdown stranded the client until the silence timeout")
	}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
}
