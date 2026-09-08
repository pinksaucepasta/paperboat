package native

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/peertransport/peerquic"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/streamauth"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/tailnet"
	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
)

type Session struct {
	owner    *Owner
	peerID   string
	quic     *peerquic.Session
	router   *peerquic.StreamRouter
	routerMu sync.Mutex
	once     sync.Once
	closing  atomic.Bool
	err      error
}

// OpenAuthorizedHTTP3 validates native network admission and exposes the
// preview-class connection exclusively to HTTP/3. The canonical operation
// credential is carried by the CONNECT request, not a competing raw stream.
func (s *Session) OpenAuthorizedHTTP3(ctx context.Context, header streamauth.Header, accessSessionID string) (http.RoundTripper, error) {
	if ctx == nil || header.Consumer != "private_http" || !s.IsPrivateHTTP3() || !s.owner.authority.Allows(s.peerID, accessSessionID, "private_access", "dial") {
		return nil, tailnet.ErrAdmission
	}
	return (&http3.Transport{}).NewClientConn(s.quic.Connection), nil
}

func (s *Session) IsPrivateHTTP3() bool {
	return s != nil && s.quic != nil && s.quic.Connection.ConnectionState().TLS.NegotiatedProtocol == PrivateHTTP3ALPN
}

func (s *Session) HTTP3Connection() (*quic.Conn, error) {
	if !s.IsPrivateHTTP3() {
		return nil, ErrInvalid
	}
	return s.quic.Connection, nil

}

func (s *Session) AuthorizeHTTP3(ctx context.Context, header streamauth.Header, authorize func(context.Context, streamauth.Header) (string, error)) error {
	if !s.IsPrivateHTTP3() || authorize == nil {
		return ErrInvalid
	}
	return s.authorizeIncoming(ctx, header, "private_access", authorize)
}

var ErrStreamRejected = errors.New("native application stream rejected")

var _ http.RoundTripper = (*http3.ClientConn)(nil)

func newSession(owner *Owner, peerID string, session *peerquic.Session) *Session {
	return &Session{owner: owner, peerID: peerID, quic: session}
}
func (s *Session) PeerID() string { return s.peerID }

func (s *Session) Open(ctx context.Context) (net.Conn, error) {
	stream, err := s.quic.Connection.OpenStreamSync(ctx)
	if err != nil {
		return nil, reconnectRequired(s.peerID, err, s.closing.Load())
	}
	return &streamConn{Stream: stream, session: s, local: directAddr("local"), remote: directAddr(s.peerID)}, nil
}
func (s *Session) Accept(ctx context.Context) (net.Conn, error) {
	s.routerMu.Lock()
	if s.router == nil {
		router, err := peerquic.NewStreamRouter(s.quic, peerquic.DevelopmentStreamRouterConfig())
		if err != nil {
			s.routerMu.Unlock()
			return nil, err
		}
		s.router = router
	}
	router := s.router
	s.routerMu.Unlock()
	stream, err := router.Accept(ctx)
	if err != nil {
		return nil, reconnectRequired(s.peerID, err, s.closing.Load())
	}
	return &routedConn{RoutedStream: stream, session: s, local: directAddr("local"), remote: directAddr(s.peerID)}, nil
}

// OpenAuthorized sends the canonical operation credential before exposing the
// stream. Network admission and application authorization remain independent.
func (s *Session) OpenAuthorized(ctx context.Context, header streamauth.Header, accessSessionID, capability string) (net.Conn, error) {
	if !s.owner.authority.Allows(s.peerID, accessSessionID, capability, "dial") {
		return nil, tailnet.ErrAdmission
	}
	encoded, err := header.MarshalBinary()
	if err != nil {
		return nil, err
	}
	connection, err := s.Open(ctx)
	if err != nil {
		return nil, err
	}
	if err := writeHeader(connection, encoded); err != nil {
		_ = connection.Close()
		return nil, err
	}
	return newLimitedConn(connection, header.MaximumBytes), nil
}

// AcceptAuthorized validates the operation credential first, then requires
// its verified access-session resource to still be in signed network scope.
func (s *Session) AcceptAuthorized(ctx context.Context, authorize func(context.Context, streamauth.Header) (string, error)) (net.Conn, streamauth.Header, error) {
	connection, err := s.Accept(ctx)
	if err != nil {
		return nil, streamauth.Header{}, err
	}
	encoded, err := readHeader(connection)
	if err != nil {
		_ = connection.Close()
		return nil, streamauth.Header{}, err
	}
	header, err := streamauth.Parse(encoded, time.Now().UTC())
	if err != nil {
		_ = connection.Close()
		return nil, streamauth.Header{}, err
	}
	capability := capabilityForConsumer(header.Consumer)
	if capability == "" {
		_ = connection.Close()
		return nil, streamauth.Header{}, tailnet.ErrAdmission
	}
	err = s.authorizeIncoming(ctx, header, capability, authorize)
	if err != nil {
		_ = connection.Close()
		return nil, streamauth.Header{}, errors.Join(ErrStreamRejected, err)
	}
	return newLimitedConn(connection, header.MaximumBytes), header, nil
}

func capabilityForConsumer(consumer string) string {
	switch consumer {
	case "terminal", "exec", "ssh":
		return "terminal"
	case "codex":
		return "codex"
	case "file_transfer", "file_transfer_key":
		return "file_transfer"
	case "private_http", "private_tcp":
		return "private_access"
	default:
		return ""
	}
}

func writeHeader(writer io.Writer, encoded []byte) error {
	if len(encoded) == 0 || len(encoded) > streamauth.MaximumHeaderSize {
		return streamauth.ErrInvalid
	}
	var size [4]byte
	binary.BigEndian.PutUint32(size[:], uint32(len(encoded)))
	if err := writeFull(writer, size[:]); err != nil {
		return err
	}
	return writeFull(writer, encoded)
}

func writeFull(writer io.Writer, value []byte) error {
	for len(value) > 0 {
		written, err := writer.Write(value)
		if err != nil {
			return err
		}
		if written < 1 || written > len(value) {
			return io.ErrShortWrite
		}
		value = value[written:]
	}
	return nil
}

func readHeader(reader io.Reader) ([]byte, error) {
	var size [4]byte
	if _, err := io.ReadFull(reader, size[:]); err != nil {
		return nil, err
	}
	length := binary.BigEndian.Uint32(size[:])
	if length == 0 || length > streamauth.MaximumHeaderSize {
		return nil, streamauth.ErrInvalid
	}
	encoded := make([]byte, length)
	_, err := io.ReadFull(reader, encoded)
	return encoded, err
}
func (s *Session) Close() error {
	s.closing.Store(true)
	s.once.Do(func() {
		s.routerMu.Lock()
		if s.router != nil {
			s.err = errors.Join(s.err, s.router.Close())
		}
		s.routerMu.Unlock()
		s.err = errors.Join(s.err, s.quic.Close())
		s.owner.remove(s)
		s.owner.emit(Event{Kind: "closed", PeerID: s.peerID, Err: s.err})
	})
	return s.err
}

type directAddr string

func (directAddr) Network() string  { return "paperboat-direct" }
func (a directAddr) String() string { return string(a) }

type streamConn struct {
	*quic.Stream
	session       *Session
	local, remote net.Addr
}

func (c *streamConn) Read(value []byte) (int, error) {
	n, err := c.Stream.Read(value)
	return n, reconnectRequired(c.session.peerID, err, c.session.closing.Load())
}
func (c *streamConn) Write(value []byte) (int, error) {
	n, err := c.Stream.Write(value)
	return n, reconnectRequired(c.session.peerID, err, c.session.closing.Load())
}
func (c *streamConn) LocalAddr() net.Addr  { return c.local }
func (c *streamConn) RemoteAddr() net.Addr { return c.remote }
func (c *streamConn) CloseWrite() error    { return c.Stream.Close() }

type routedConn struct {
	*peerquic.RoutedStream
	session       *Session
	local, remote net.Addr
}

func (c *routedConn) Read(value []byte) (int, error) {
	n, err := c.RoutedStream.Read(value)
	return n, reconnectRequired(c.session.peerID, err, c.session.closing.Load())
}
func (c *routedConn) Write(value []byte) (int, error) {
	n, err := c.RoutedStream.Write(value)
	return n, reconnectRequired(c.session.peerID, err, c.session.closing.Load())
}

type limitedConn struct {
	net.Conn
	readRemaining  atomic.Uint64
	writeRemaining atomic.Uint64
}

func newLimitedConn(connection net.Conn, maximum uint64) net.Conn {
	limited := &limitedConn{Conn: connection}
	limited.readRemaining.Store(maximum)
	limited.writeRemaining.Store(maximum)
	return limited
}

func (c *limitedConn) Read(value []byte) (int, error) {
	if len(value) == 0 {
		return 0, nil
	}
	reserved := reserve(&c.readRemaining, uint64(len(value)), true)
	if reserved == 0 {
		return 0, streamauth.ErrInvalid
	}
	value = value[:reserved]
	read, err := c.Conn.Read(value)
	c.readRemaining.Add(reserved - uint64(read))
	return read, err
}

func (c *limitedConn) Write(value []byte) (int, error) {
	reserved := reserve(&c.writeRemaining, uint64(len(value)), false)
	if reserved != uint64(len(value)) {
		return 0, streamauth.ErrInvalid
	}
	written, err := c.Conn.Write(value)
	c.writeRemaining.Add(reserved - uint64(written))
	return written, err
}

func reserve(remaining *atomic.Uint64, requested uint64, partial bool) uint64 {
	for {
		available := remaining.Load()
		amount := requested
		if amount > available {
			if !partial {
				return 0
			}
			amount = available
		}
		if amount == 0 || remaining.CompareAndSwap(available, available-amount) {
			return amount
		}
	}
}

func (c *limitedConn) CloseWrite() error {
	if closer, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return closer.CloseWrite()
	}
	return c.Conn.Close()
}

func (c *routedConn) LocalAddr() net.Addr  { return c.local }
func (c *routedConn) RemoteAddr() net.Addr { return c.remote }
func (c *routedConn) CloseWrite() error    { return c.RoutedStream.Close() }

var _ net.Conn = (*streamConn)(nil)
var _ net.Conn = (*routedConn)(nil)
var _ interface{ CloseWrite() error } = (*streamConn)(nil)

// Credential validation precedes any control-plane refresh. A new, valid
// operation may have committed after the host's last signed network snapshot.
func (s *Session) authorizeIncoming(ctx context.Context, header streamauth.Header, capability string, authorize func(context.Context, streamauth.Header) (string, error)) error {
	resourceID, err := authorize(ctx, header)
	if err != nil {
		return errors.Join(tailnet.ErrAdmission, err)
	}
	if s.owner.authority.Allows(s.peerID, resourceID, capability, "accept") {
		return nil
	}
	if s.owner.refreshAuthority != nil {
		if err := s.owner.refreshAuthority(ctx); err != nil {
			return errors.Join(tailnet.ErrAdmission, err)
		}
	}
	if !s.owner.authority.Allows(s.peerID, resourceID, capability, "accept") {
		return tailnet.ErrAdmission
	}
	return nil
}
