package edgehttp

import (
	"context"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/shared/addrlimit"
	"github.com/pinksaucepasta/paperboat/shared/connectorprotocol"
	"github.com/pinksaucepasta/paperboat/shared/edgeports"
)

type remoteConn struct {
	net.Conn
	remote net.Addr
}

func (c remoteConn) RemoteAddr() net.Addr { return c.remote }

type queuedListener struct {
	conns  chan net.Conn
	closed chan struct{}
	once   sync.Once
}

func (l *queuedListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}
func (l *queuedListener) Close() error   { l.once.Do(func() { close(l.closed) }); return nil }
func (l *queuedListener) Addr() net.Addr { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)} }

// sharedTCPFixture serves one raw TCP publication whose forwards block until the test ends.
func sharedTCPFixture(t *testing.T) (queue func(source string) net.Conn, forwarded *atomic.Int32) {
	t.Helper()
	authority := &publicTCPAuthorityFixture{}
	authority.set(publicTCPListenerDecision(int(edgeports.PublicTCPMin), "listener_1"))
	routes, err := NewDataCarrierRouteRegistry(DataCarrierRouteRegistryConfig{})
	if err != nil {
		t.Fatal(err)
	}
	listener := &queuedListener{conns: make(chan net.Conn, 4096), closed: make(chan struct{})}
	manager, err := NewPublicTCPListeners(PublicTCPListenerConfig{ListenHost: "127.0.0.1", Authority: authority, Routes: routes, Interval: time.Hour,
		Listen: func(string, string) (net.Listener, error) { return listener, nil }})
	if err != nil {
		t.Fatal(err)
	}
	forwarded = new(atomic.Int32)
	release := make(chan struct{})
	manager.forward = func(_ context.Context, c net.Conn, _ connectorprotocol.IngressDecision, _ func(context.Context, connectorprotocol.IngressDecision) (connectorprotocol.IngressDecision, error)) error {
		forwarded.Add(1)
		<-release
		return c.Close()
	}
	if err = manager.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { close(release); manager.Shutdown(context.Background()) })
	return func(source string) net.Conn {
		client, server := net.Pipe()
		t.Cleanup(func() { _ = client.Close() })
		listener.conns <- remoteConn{Conn: server, remote: &net.TCPAddr{IP: net.ParseIP(source), Port: 40000}}
		return client
	}, forwarded
}

func shed(t *testing.T, client net.Conn) bool {
	t.Helper()
	_ = client.SetReadDeadline(time.Now().Add(time.Second))
	_, err := client.Read(make([]byte, 1))
	return err != nil && !isTimeout(err)
}

func isTimeout(err error) bool { n, ok := err.(net.Error); return ok && n.Timeout() }

func waitForwarded(t *testing.T, n *atomic.Int32, want int32) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); n.Load() != want; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("forwarded %d connections, want %d", n.Load(), want)
		}
	}
}

func TestPublicTCPSourceAddressCannotHoldMoreThanItsShare(t *testing.T) {
	queue, forwarded := sharedTCPFixture(t)
	for range addrlimit.Streams {
		queue("203.0.113.7")
	}
	waitForwarded(t, forwarded, addrlimit.Streams)
	if !shed(t, queue("203.0.113.7")) {
		t.Fatal("a connection past one address's share was served")
	}
	queue("203.0.113.8")
	waitForwarded(t, forwarded, addrlimit.Streams+1)
}

// A popular service holds far more than 1,024 connections: no publication-wide cap sheds its visitors.
func TestPublicTCPPublicationIsNotCapped(t *testing.T) {
	queue, forwarded := sharedTCPFixture(t)
	const visitors = 1500
	for i := range visitors {
		queue(fmt.Sprintf("10.%d.%d.1", i/250, i%250))
	}
	waitForwarded(t, forwarded, visitors)
}
