package peerquic_test

import (
	"bytes"
	"context"
	"errors"
	"net"
	"sync"
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
	// The carrier owner closes its session before the listener; both orders
	// must keep the queued close until it has left.
	for _, sessionFirst := range []bool{false, true} {
		testListenerShutdownDeliversRemoteApplicationClose(t, sessionFirst)
	}
}

func testListenerShutdownDeliversRemoteApplicationClose(t *testing.T, sessionFirst bool) {
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
	// The close must also survive a path that queues writes and drops them when
	// its network is torn down, as the carrier's netstack/WireGuard network does.
	path := &queuedPacketSocket{restartPacketSocket: &restartPacketSocket{serverUDP, clientUDP.LocalAddr()}}
	listener, err := peerquic.ListenPacket(path, serverTLS, config)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	accepted := make(chan *peerquic.Session, 1)
	go func() { session, _ := listener.Accept(ctx); accepted <- session }()
	client, err := peerquic.DialPacket(ctx, &restartPacketSocket{clientUDP, serverUDP.LocalAddr()}, clientTLS, config)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	server := <-accepted
	if server == nil {
		t.Fatal("listener did not accept the client")
	}
	closed := make(chan error, 1)
	go func() {
		if sessionFirst {
			_ = server.Close()
		}
		started := time.Now()
		err := listener.Close()
		if time.Since(started) > 50*time.Millisecond {
			err = errors.New("listener close waited for the closing period")
		}
		// The carrier tears its network down only after the closing deadline.
		time.Sleep(time.Until(listener.ClosingDeadline()))
		path.teardown()
		closed <- err
	}()
	select {
	case <-client.Connection.Context().Done():
		var remote *quic.ApplicationError
		if !errors.As(context.Cause(client.Connection.Context()), &remote) || !remote.Remote {
			t.Fatal("listener shutdown did not deliver an authenticated remote application close")
		}
	case <-time.After(time.Second):
		t.Fatal("listener shutdown stranded the client until the silence timeout", sessionFirst)
	}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	if server.ClosingPeriod() <= 0 || client.ClosingPeriod() != 0 {
		t.Fatal("closing period must cover only the side that sent the close")
	}
}

// queuedPacketSocket models a userspace path: a write is only queued. Closing
// the socket keeps what is queued; tearing the network down drops it.
type queuedPacketSocket struct {
	*restartPacketSocket
	mu             sync.Mutex
	socketClosed   bool
	networkStopped bool
}

func (p *queuedPacketSocket) WriteTo(value []byte, addr net.Addr) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.socketClosed {
		return 0, net.ErrClosed
	}
	packet := bytes.Clone(value)
	time.AfterFunc(20*time.Millisecond, func() {
		p.mu.Lock()
		defer p.mu.Unlock()
		if !p.networkStopped {
			_, _ = p.restartPacketSocket.WriteTo(packet, addr)
		}
	})
	return len(value), nil
}

func (p *queuedPacketSocket) Close() error {
	p.mu.Lock()
	p.socketClosed = true
	p.mu.Unlock()
	return p.SetReadDeadline(time.Now())
}

func (p *queuedPacketSocket) teardown() {
	p.mu.Lock()
	p.networkStopped = true
	p.mu.Unlock()
	_ = p.restartPacketSocket.Close()
}
