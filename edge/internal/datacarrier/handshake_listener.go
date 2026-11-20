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

	"github.com/pinksaucepasta/paperboat/shared/addrlimit"
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
	ready    chan net.Conn
	done     chan struct{}
	failed   chan error
	pending  *addrlimit.Limiter // unauthenticated handshakes in flight per address
	stopOnce sync.Once
}

// pendingHandshakesPerAddress bounds one address's unfinished TLS handshakes. It bounds an address, not the
// node: a peer that opens sockets and stalls pins at most this many; others handshake normally. Prior art:
// Tailscale's derper gives each client a handshake deadline (derp/derpserver), cloudflared a HandshakeIdleTimeout.
const pendingHandshakesPerAddress = 16

func newHandshakeListener(inner net.Listener, config *tls.Config, timeout time.Duration, observe func(error)) *handshakeListener {
	l := &handshakeListener{Listener: inner, config: config.Clone(), timeout: timeout, observe: observe, ready: make(chan net.Conn), done: make(chan struct{}), failed: make(chan error, 1), pending: addrlimit.NewInFlight(pendingHandshakesPerAddress)}
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
		release, ok := l.pending.Admit(remoteIP(raw), false)
		if !ok {
			_ = raw.Close()
			continue
		}
		go l.handshake(raw, release) // each handshake ends at the timeout
	}
}

func (l *handshakeListener) handshake(raw net.Conn, release func()) {
	counted := &readCountingConn{Conn: raw}
	// A refused handshake leaves no peer state on the connection, so the check's own state is kept.
	config, seen := l.config, tls.ConnectionState{}
	if verify := l.config.VerifyConnection; verify != nil {
		config = l.config.Clone()
		config.VerifyConnection = func(state tls.ConnectionState) error { seen = state; return verify(state) }
	}
	connection := tls.Server(counted, config)
	ctx, cancel := context.WithTimeout(context.Background(), l.timeout)
	err := connection.HandshakeContext(ctx)
	cancel()
	release()
	if err != nil {
		_ = raw.Close()
		if counted.read.Load() > 0 {
			failure := &handshakeFailure{error: errCarrierTLSHandshake, thumbprint: peerThumbprint(seen), remote: remoteIP(raw)}
			var reason CarrierRefusal
			if errors.As(err, &reason) {
				failure.error = fmt.Errorf("%w: %w", errCarrierTLSHandshake, reason)
			}
			l.observe(failure)
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

// handshakeFailure is a failed handshake with who dialled: the thumbprint of the client certificate's key
// (what the machines table holds as public_identity_key, so a refusal can be traced to a machine; empty when
// the peer sent none) and its IP. Both are public values, never secrets.
type handshakeFailure struct {
	error
	thumbprint, remote string
}

func (f *handshakeFailure) Unwrap() error { return f.error }

// FailurePeer is the key thumbprint and remote IP of a failed carrier handshake, empty for any other failure.
func FailurePeer(err error) (thumbprint, remote string) {
	var failure *handshakeFailure
	if errors.As(err, &failure) {
		return failure.thumbprint, failure.remote
	}
	return "", ""
}

func remoteIP(c net.Conn) string {
	host, _, err := net.SplitHostPort(c.RemoteAddr().String())
	if err != nil {
		return c.RemoteAddr().String()
	}
	return host
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
