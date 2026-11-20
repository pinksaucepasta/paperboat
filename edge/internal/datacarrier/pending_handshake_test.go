package datacarrier

import (
	"crypto/tls"
	"net"
	"testing"
	"time"
)

type remoteConn struct {
	net.Conn
	remote net.Addr
}

func (c remoteConn) RemoteAddr() net.Addr { return c.remote }

type queuedListener struct {
	conns  chan net.Conn
	closed chan struct{}
}

func (l *queuedListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}
func (l *queuedListener) Close() error   { close(l.closed); return nil }
func (l *queuedListener) Addr() net.Addr { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)} }

// One address that opens sockets and never speaks TLS pins at most its share of handshakes; another
// address still handshakes.
func TestHandshakeListenerBoundsStalledHandshakesPerAddress(t *testing.T) {
	_, serverTLS, _ := testEndpointCertificates(t)
	serverTLS.ClientAuth = tls.NoClientCert
	inner := &queuedListener{conns: make(chan net.Conn, 64), closed: make(chan struct{})}
	listener := newHandshakeListener(inner, serverTLS, time.Minute, func(error) {})
	defer listener.Close()
	dial := func(source string) net.Conn {
		client, server := net.Pipe()
		t.Cleanup(func() { _ = client.Close() })
		inner.conns <- remoteConn{Conn: server, remote: &net.TCPAddr{IP: net.ParseIP(source), Port: 40000}}
		return client
	}
	var stalled []net.Conn
	for range pendingHandshakesPerAddress + 1 {
		stalled = append(stalled, dial("203.0.113.7"))
	}
	over := stalled[pendingHandshakesPerAddress]
	_ = over.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := over.Read(make([]byte, 1)); err == nil || isTimeout(err) {
		t.Fatalf("a handshake past one address's share was held open: %v", err)
	}
	client := tls.Client(dial("203.0.113.8"), &tls.Config{InsecureSkipVerify: true, ServerName: "localhost", MinVersion: tls.VersionTLS13})
	done := make(chan error, 1)
	go func() { done <- client.Handshake() }()
	accepted := make(chan error, 1)
	go func() { _, err := listener.Accept(); accepted <- err }()
	for _, result := range []chan error{done, accepted} {
		select {
		case err := <-result:
			if err != nil {
				t.Fatalf("second address's handshake: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("second address's handshake did not complete")
		}
	}
}

func isTimeout(err error) bool { n, ok := err.(net.Error); return ok && n.Timeout() }
