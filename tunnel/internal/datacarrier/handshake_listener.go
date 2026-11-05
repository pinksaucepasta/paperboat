package datacarrier

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

var errCarrierTLSHandshake = errors.New("data carrier TLS handshake failed")

// handshakeListener completes TLS before net/http sees a connection, so the
// failure is typed here instead of becoming a formatted net/http log line.
// Machines measure edge latency with a bare TCP connect and close; a peer that
// closes without sending a byte is not a failure and is dropped silently.
type handshakeListener struct {
	net.Listener
	config   *tls.Config
	timeout  time.Duration
	observe  func(error)
	pending  chan struct{}
	ready    chan net.Conn
	done     chan struct{}
	failed   chan error
	stopOnce sync.Once
}

func newHandshakeListener(inner net.Listener, config *tls.Config, timeout time.Duration, maximumPending int, observe func(error)) *handshakeListener {
	l := &handshakeListener{Listener: inner, config: config.Clone(), timeout: timeout, observe: observe, pending: make(chan struct{}, maximumPending), ready: make(chan net.Conn), done: make(chan struct{}), failed: make(chan error, 1)}
	go l.acceptLoop()
	return l
}

func (l *handshakeListener) acceptLoop() {
	for {
		raw, err := l.Listener.Accept()
		if err != nil {
			l.failed <- err
			return
		}
		select {
		case l.pending <- struct{}{}:
			go l.handshake(raw)
		default:
			_ = raw.Close() // bounded: too many unfinished handshakes
		}
	}
}

func (l *handshakeListener) handshake(raw net.Conn) {
	defer func() { <-l.pending }()
	counted := &readCountingConn{Conn: raw}
	connection := tls.Server(counted, l.config)
	ctx, cancel := context.WithTimeout(context.Background(), l.timeout)
	err := connection.HandshakeContext(ctx)
	cancel()
	if err != nil {
		_ = raw.Close()
		if counted.read.Load() > 0 {
			var reason CarrierRefusal
			if errors.As(err, &reason) {
				l.observe(fmt.Errorf("%w: %w", errCarrierTLSHandshake, reason))
			} else {
				l.observe(errCarrierTLSHandshake)
			}
		}
		return
	}
	select {
	case l.ready <- connection:
	case <-l.done:
		_ = connection.Close()
	}
}

func (l *handshakeListener) Accept() (net.Conn, error) {
	select {
	case connection := <-l.ready:
		return connection, nil
	case err := <-l.failed:
		l.failed <- err
		return nil, err
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *handshakeListener) Close() error {
	l.stopOnce.Do(func() { close(l.done) })
	return l.Listener.Close()
}

type readCountingConn struct {
	net.Conn
	read atomic.Int64
}

func (c *readCountingConn) Read(data []byte) (int, error) {
	n, err := c.Conn.Read(data)
	c.read.Add(int64(n))
	return n, err
}
