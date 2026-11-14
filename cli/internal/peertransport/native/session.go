package native

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"github.com/pinksaucepasta/paperboat/internal/diagnostics"
	"github.com/pinksaucepasta/paperboat/shared/peerconsumer"
	"github.com/pinksaucepasta/paperboat/shared/serverclock"
	"io"
	"net"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/peertransport/peerquic"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/streamauth"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/tailnet"
	"github.com/pinksaucepasta/paperboat/shared/admission"
	"github.com/pinksaucepasta/paperboat/shared/diagnosticlog"
	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
)

type Session struct {
	traceLabel   string
	terminalUsed atomic.Bool
	owner        *Owner
	peerID       string
	binding      tailnet.NetworkBinding
	quic         *peerquic.Session
	router       *peerquic.StreamRouter
	routerMu     sync.Mutex
	once         sync.Once
	closing      atomic.Bool
	err          error
	scopeMu      sync.Mutex
	http3Scope   *scopeKey
	carrier      *admission.Lease

	// A shared session (Owner.DialShared) is one QUIC association many callers open streams on. The association itself
	// counts its handles; a handle is a *Session whose base is the association, and closing a handle only releases it.
	handlesMu sync.Mutex
	handles   int
	dead      bool
	base      *Session
}

// MeterBinding contains only verified accounting metadata, never credentials.
type MeterBinding struct {
	AccessSessionID, StreamID, Consumer string
	// PeerAccountID is set for a stream admitted from the host's own rules (an offline header): the server
	// attributes its usage from the peer's access to the host, as there is no access session.
	PeerAccountID string
	Reverse       bool
	Path          func(string) (string, string)
}

// MeterIncoming measures authorized host application I/O. Reverse is used for
// an origin connection, whose writes carry client uploads rather than downloads.
func (s *Session) MeterIncoming(conn net.Conn, header streamauth.Header, accessSessionID string, reverse bool) net.Conn {
	if header.Consumer == "config_compare" {
		accessSessionID = header.UsageSessionID
	}
	if s.owner.meterIncoming == nil {
		return conn
	}
	peerAccountID := ""
	if header.Offline {
		peerAccountID = s.binding.AccountID
	}
	return s.owner.meterIncoming(conn, MeterBinding{AccessSessionID: accessSessionID, StreamID: header.StreamID, Consumer: header.Consumer, PeerAccountID: peerAccountID, Reverse: reverse, Path: func(direction string) (string, string) {
		path, err := s.owner.authority.PeerUsagePathDirection(s.peerID, direction)
		if err != nil {
			return "unknown", ""
		}
		return path.Mode, path.NodeID
	}})
}

// OpenAuthorizedHTTP3 validates native network admission and exposes the
// preview-class connection exclusively to HTTP/3. The canonical operation
// credential is carried by the CONNECT request, not a competing raw stream.
func (s *Session) OpenAuthorizedHTTP3(ctx context.Context, header streamauth.Header, accessSessionID string) (http.RoundTripper, error) {
	if ctx == nil || header.Consumer != "private_http" || !s.IsPrivateHTTP3() || !s.owner.authority.Allows(s.peerID, accessSessionID, "private_access", "dial") {
		return nil, tailnet.ErrAdmission
	}
	if err := s.bindHTTP3Scope(accessSessionID, "dial", time.Unix(header.DeadlineUnix, 0)); err != nil {
		return nil, err
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
	var resource string
	_, err := s.authorizeIncoming(ctx, header, "private_access", func(ctx context.Context, h streamauth.Header) (string, error) {
		var err error
		resource, err = authorize(ctx, h)
		return resource, err
	})
	if err != nil {
		return err
	}
	return s.bindHTTP3Scope(resource, "accept", time.Unix(header.DeadlineUnix, 0))
}

var ErrStreamRejected = errors.New("native application stream rejected")

var _ http.RoundTripper = (*http3.ClientConn)(nil)

func newSession(owner *Owner, peerID string, binding tailnet.NetworkBinding, session *peerquic.Session) (*Session, error) {
	s := &Session{owner: owner, peerID: peerID, binding: binding, quic: session}
	release, backing, err := owner.authority.TrackAdmittedCarrier(peerID)
	if err != nil {
		return nil, err
	}
	lease, err := admission.Admit(session.Connection.Context(), time.Time{}, nil)
	if err != nil {
		release()
		return nil, err
	}
	lease.OnDone(release)
	if err := lease.Watch(owner.authority.ScopeChanges, func() bool { return owner.authority.AllowsAdmittedCarrier(peerID, binding, backing) }); err != nil {
		lease.Close()
		return nil, err
	}
	lease.Bind(session)
	s.carrier = lease
	if diagnostics.TerminalHopsEnabled() {
		s.traceLabel = diagnostics.ConnectionHopLabel(session.Connection.LocalAddr().String(), session.Connection.RemoteAddr().String())
	}
	return s, nil
}
func (s *Session) PeerID() string { return s.peerID }

// Peer is the account and endpoint a native session proved.
type Peer struct{ AccountID, EndpointID string }

type peerContextKey struct{}

// ContextWithPeer names the session's peer in ctx, for the host application that serves its streams.
func ContextWithPeer(ctx context.Context, s *Session) context.Context {
	return context.WithValue(ctx, peerContextKey{}, Peer{AccountID: s.binding.AccountID, EndpointID: s.binding.EndpointID})
}

// PeerFromContext returns the peer ContextWithPeer named.
func PeerFromContext(ctx context.Context) (Peer, bool) {
	peer, ok := ctx.Value(peerContextKey{}).(Peer)
	return peer, ok
}

func (s *Session) Open(ctx context.Context) (net.Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := s.quic.Connection.Context().Err(); err != nil {
		return nil, reconnectRequired(s.peerID, context.Cause(s.quic.Connection.Context()), s.closing.Load())
	}
	peer, err := s.owner.authority.Peer(s.peerID)
	if err != nil {
		_ = s.Close()
		return nil, err
	}
	if peer != s.binding {
		_ = s.Close()
		return nil, tailnet.ErrAdmission
	}
	stream, err := s.quic.Connection.OpenStreamSync(ctx)
	if err != nil {
		return nil, reconnectRequired(s.peerID, err, s.closing.Load())
	}
	return &streamConn{Stream: stream, session: s, local: directAddr("local"), remote: directAddr(s.peerID)}, nil
}

// ensureRouterLocked starts reading incoming streams.
func (s *Session) ensureRouterLocked() error {
	if s.router != nil {
		return nil
	}
	router, err := peerquic.NewStreamRouter(s.quic, peerquic.DevelopmentStreamRouterConfig())
	if err != nil {
		return err
	}
	s.router = router
	return nil
}

func (s *Session) Accept(ctx context.Context) (net.Conn, error) {
	s.routerMu.Lock()
	err := s.ensureRouterLocked()
	router := s.router
	s.routerMu.Unlock()
	if err != nil {
		return nil, err
	}
	stream, err := router.Accept(ctx)
	if err != nil {
		return nil, reconnectRequired(s.peerID, err, s.closing.Load())
	}
	return &routedConn{RoutedStream: stream, session: s, local: directAddr("local"), remote: directAddr(s.peerID)}, nil
}

// OpenAuthorized sends the canonical operation credential before exposing the
// stream. Network admission and application authorization remain independent.
func (s *Session) OpenAuthorized(ctx context.Context, header streamauth.Header, accessSessionID, capability string) (net.Conn, error) {
	resource := accessSessionID
	if !s.owner.authority.Allows(s.peerID, resource, capability, "dial") {
		// An access session this document does not hold (a new operation's, committed after the last refresh; a
		// resume's that left the signed scope) is admitted on the host by its standing rule for this peer
		// (standingResource); the client dials on the same rule, and only a withdrawn rule refuses it here.
		resource = s.dialStandingResource(header, capability)
		if resource == "" {
			return nil, tailnet.ErrAdmission
		}
	}
	encoded, err := header.MarshalBinary()
	if err != nil {
		return nil, err
	}
	return s.openAdmitted(ctx, header, encoded, resource, capability)
}

// dialStandingResource is the scope that stands for the stream's access session (tailnet.Authority.DialStanding): a
// new terminal, exec or inspector stream, or a terminal resume; empty otherwise.
func (s *Session) dialStandingResource(header streamauth.Header, capability string) string {
	if header.Resume != "" && capability != "terminal" {
		return ""
	}
	return s.owner.authority.DialStanding(s.peerID, s.binding.MachineID, capability)
}

// dialAdmits reports whether resource still admits a dial of capability: its signed scope, or the personal mesh pair
// that stands for it.
func (s *Session) dialAdmits(resource, capability string) bool {
	return s.owner.authority.Allows(s.peerID, resource, capability, "dial") || resource == s.owner.authority.PersonalScope(s.peerID) && resource != ""
}

// AcceptAuthorized validates the operation credential first, then requires
// its verified access-session resource to still be in signed network scope.
func (s *Session) AcceptAuthorized(ctx context.Context, authorize func(context.Context, streamauth.Header) (string, error)) (net.Conn, streamauth.Header, error) {
	connection, err := s.Accept(ctx)
	if err != nil {
		return nil, streamauth.Header{}, err
	}
	return s.Admit(ctx, connection, authorize)
}

// Admit is the admission half of AcceptAuthorized for a stream Accept already returned. A listener calls it from the
// stream's own goroutine: admission reads the credential header and asks the server, so doing it inside the accept
// loop admitted one stream at a time (about two seconds each; a client got ~44 terminals through before their
// credentials expired, F21-164).
func (s *Session) Admit(ctx context.Context, connection net.Conn, authorize func(context.Context, streamauth.Header) (string, error)) (net.Conn, streamauth.Header, error) {
	encoded, err := readAuthorizationHeader(connection)
	if err != nil {
		return rejectAuthorizedStream(connection, err)
	}
	header, err := streamauth.Parse(encoded, serverclock.Now().UTC())
	if err != nil {
		return rejectAuthorizedStream(connection, err)
	}
	capability := peerconsumer.NetworkCapability(header.Consumer)
	if capability == "" {
		return rejectAuthorizedStream(connection, tailnet.ErrAdmission)
	}
	if header.Offline {
		admitted, err := s.acceptOffline(ctx, connection, header, capability)
		if err != nil {
			// Offline SSH already sent its typed refusal in place of a banner.
			if header.Consumer == "ssh" {
				return nil, streamauth.Header{}, errors.Join(ErrStreamRejected, err, connection.Close())
			}
			return rejectAuthorizedStream(connection, err)
		}
		return admitted, header, nil
	}
	if header.Resume != "" {
		admitted, err := s.acceptResume(ctx, connection, header)
		if err != nil {
			return rejectAuthorizedStream(connection, err)
		}
		return admitted, header, nil
	}
	var accessSessionID string
	scopeResource, err := s.authorizeIncoming(ctx, header, capability, func(ctx context.Context, header streamauth.Header) (string, error) {
		var authorizeErr error
		accessSessionID, authorizeErr = authorize(ctx, header)
		return accessSessionID, authorizeErr
	})
	if err != nil {
		return rejectAuthorizedStream(connection, err)
	}
	scoped, err := s.bindScope(newLimitedConn(connection, header.MaximumBytes), scopeResource, capability, "accept", time.Unix(header.DeadlineUnix, 0))
	if err != nil {
		return rejectAuthorizedStream(connection, err)
	}
	if header.Consumer == "terminal" {
		s.terminalUsed.Store(true)
	}
	if err := acknowledgeAdmission(connection); err != nil {
		return nil, streamauth.Header{}, errors.Join(ErrStreamRejected, err, scoped.Close())
	}
	return s.MeterIncoming(scoped, header, accessSessionID, false), header, nil
}

// Header parsing is per-stream admission. A bad or abandoned header must
// neither tear down other applications nor hold the accept loop indefinitely.
const authorizationHeaderTimeout = 5 * time.Second

func readAuthorizationHeader(connection net.Conn) ([]byte, error) {
	if err := connection.SetReadDeadline(time.Now().Add(authorizationHeaderTimeout)); err != nil {
		return nil, err
	}
	encoded, err := readHeader(connection)
	return encoded, errors.Join(err, connection.SetReadDeadline(time.Time{}))
}

func rejectAuthorizedStream(connection net.Conn, err error) (net.Conn, streamauth.Header, error) {
	reason := "denied"
	switch {
	case errors.Is(err, streamauth.ErrExpired):
		reason = "deadline_passed"
	case errors.Is(err, streamauth.ErrInvalid):
		reason = "header_invalid"
	}
	// The refusal reaches the client as one stream code; the host log keeps why (F21-02). No error text: it may
	// carry verifier detail.
	diagnosticlog.TryWarn("native stream admission refused", "reason", reason, "server_clock_offset", serverclock.Offset().String())
	code := AdmissionDenied
	var refusal *AdmissionRefusal
	if errors.As(err, &refusal) && refusal.Code == AdmissionUnavailable {
		code = AdmissionUnavailable
	}
	if stream, ok := connection.(interface {
		CancelRead(quic.StreamErrorCode)
		CancelWrite(quic.StreamErrorCode)
	}); ok {
		stream.CancelWrite(code)
		stream.CancelRead(code)
	}
	return nil, streamauth.Header{}, errors.Join(ErrStreamRejected, err, connection.Close())
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

// share returns a new handle on the association, or false once it is closing, closed or its last handle left.
func (s *Session) share() (*Session, bool) {
	s.handlesMu.Lock()
	defer s.handlesMu.Unlock()
	if s.dead || s.closing.Load() || s.quic.Connection.Context().Err() != nil {
		return nil, false
	}
	s.handles++
	s.routerMu.Lock()
	router := s.router
	s.routerMu.Unlock()
	return &Session{traceLabel: s.traceLabel, owner: s.owner, peerID: s.peerID, binding: s.binding, quic: s.quic, router: router, carrier: s.carrier, base: s}, true
}

// release is a handle's Close: the association closes with its last handle.
func (s *Session) release() {
	s.handlesMu.Lock()
	s.handles--
	last := s.handles <= 0
	if last {
		s.dead = true
	}
	s.handlesMu.Unlock()
	if last {
		_ = s.Close()
	}
}

func (s *Session) Close() error {
	if s.base != nil { // a handle: it ends its own use, never the association other callers share
		s.once.Do(s.base.release)
		return nil
	}
	s.closing.Store(true)
	s.once.Do(func() {
		s.err = errors.Join(s.err, s.quic.Close())
		if s.carrier != nil {
			s.carrier.Close()
		}
		s.routerMu.Lock()
		if s.router != nil {
			s.err = errors.Join(s.err, s.router.Close())
		}
		s.routerMu.Unlock()
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
	readBytes     atomic.Uint64
	writeBytes    atomic.Uint64
}

func (c *streamConn) Read(value []byte) (int, error) {
	diagnostics.TerminalHop("quic_stream_read_start", c.session.traceLabel+"_"+strconv.FormatInt(int64(c.Stream.StreamID()), 10), c.readBytes.Load(), c.readBytes.Load()+uint64(len(value)), time.Now())
	n, err := c.Stream.Read(value)
	if n > 0 {
		end := c.readBytes.Add(uint64(n))
		diagnostics.TerminalHop("quic_stream_read_end", c.session.traceLabel+"_"+strconv.FormatInt(int64(c.Stream.StreamID()), 10), end-uint64(n), end, time.Now())
	}
	return n, reconnectRequired(c.session.peerID, err, c.session.closing.Load())
}
func (c *streamConn) Write(value []byte) (int, error) {
	diagnostics.TerminalHop("quic_stream_write_start", c.session.traceLabel+"_"+strconv.FormatInt(int64(c.Stream.StreamID()), 10), c.writeBytes.Load(), c.writeBytes.Load()+uint64(len(value)), time.Now())
	n, err := c.Stream.Write(value)
	if n > 0 {
		end := c.writeBytes.Add(uint64(n))
		diagnostics.TerminalHop("quic_stream_write_end", c.session.traceLabel+"_"+strconv.FormatInt(int64(c.Stream.StreamID()), 10), end-uint64(n), end, time.Now())
	}
	return n, reconnectRequired(c.session.peerID, err, c.session.closing.Load())
}
func (c *streamConn) LocalAddr() net.Addr  { return c.local }
func (c *streamConn) RemoteAddr() net.Addr { return c.remote }

// closeSendSide sends the FIN. When the peer already stopped reading, its STOP_SENDING cancels our send side with a
// remote *quic.StreamError and quic-go's Close then fails with a plain error ("close called for canceled stream",
// send_stream.go): the peer has what it wanted and the FIN is moot, so exactly that case is the stream already being
// closed (net.ErrClosed), which the SSH bridge treats as the client's read side being done. Any other failure of Close
// (a local cancel, a lost connection, anything else) stays an error. Both conn kinds use it: streamConn (the dial path)
// and routedConn (the accept path, which Host.Serve audits through).
func closeSendSide(stream *quic.Stream) error {
	err := stream.Close()
	if err == nil {
		return nil
	}
	var cancelled *quic.StreamError
	if errors.As(context.Cause(stream.Context()), &cancelled) && cancelled.Remote {
		return fmt.Errorf("the peer stopped reading the stream: %w", net.ErrClosed)
	}
	return err
}

func (c *streamConn) CloseWrite() error { return closeSendSide(c.Stream) }

type routedConn struct {
	*peerquic.RoutedStream
	session       *Session
	local, remote net.Addr
	readBytes     atomic.Uint64
	writeBytes    atomic.Uint64
}

func (c *routedConn) Read(value []byte) (int, error) {
	diagnostics.TerminalHop("quic_stream_read_start", c.session.traceLabel+"_"+strconv.FormatInt(int64(c.RoutedStream.StreamID()), 10), c.readBytes.Load(), c.readBytes.Load()+uint64(len(value)), time.Now())
	n, err := c.RoutedStream.Read(value)
	if n > 0 {
		end := c.readBytes.Add(uint64(n))
		diagnostics.TerminalHop("quic_stream_read_end", c.session.traceLabel+"_"+strconv.FormatInt(int64(c.RoutedStream.StreamID()), 10), end-uint64(n), end, time.Now())
	}
	return n, reconnectRequired(c.session.peerID, err, c.session.closing.Load())
}
func (c *routedConn) Write(value []byte) (int, error) {
	diagnostics.TerminalHop("quic_stream_write_start", c.session.traceLabel+"_"+strconv.FormatInt(int64(c.RoutedStream.StreamID()), 10), c.writeBytes.Load(), c.writeBytes.Load()+uint64(len(value)), time.Now())
	n, err := c.RoutedStream.Write(value)
	if n > 0 {
		end := c.writeBytes.Add(uint64(n))
		diagnostics.TerminalHop("quic_stream_write_end", c.session.traceLabel+"_"+strconv.FormatInt(int64(c.RoutedStream.StreamID()), 10), end-uint64(n), end, time.Now())
	}
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
func (c *routedConn) CloseWrite() error    { return closeSendSide(c.RoutedStream.Stream) }

var _ net.Conn = (*streamConn)(nil)
var _ net.Conn = (*routedConn)(nil)
var _ interface{ CloseWrite() error } = (*streamConn)(nil)

// authorizeIncoming validates the operation credential, then returns the resource whose signed scope admits the stream:
// the credential's access session when the host already holds its scope, else the host itself when its standing rule
// admits the peer (terminal and exec), else the access session after one on-demand refresh of the host's signed state (a new, valid operation may have
// committed after the host's last signed network snapshot).
func (s *Session) authorizeIncoming(ctx context.Context, header streamauth.Header, capability string, authorize func(context.Context, streamauth.Header) (string, error)) (string, error) {
	resourceID, err := authorize(ctx, header)
	if err != nil {
		diagnosticlog.TryWarn("native stream admission refused", "reason", "application_authorize")
		return "", errors.Join(tailnet.ErrAdmission, err)
	}
	if s.owner.authority.Allows(s.peerID, resourceID, capability, "accept") {
		return resourceID, nil
	}
	if host := s.standingResource(capability); host != "" {
		return host, nil
	}
	if s.owner.refreshAuthority != nil {
		if err := s.owner.refreshAuthority(ctx); err != nil {
			diagnosticlog.TryWarn("native stream admission refused", "reason", "authority_refresh")
			return "", errors.Join(tailnet.ErrAdmission, err)
		}
	}
	if !s.owner.authority.Allows(s.peerID, resourceID, capability, "accept") {
		diagnosticlog.TryWarn("native stream admission refused", "reason", "authority_scope")
		return "", tailnet.ErrAdmission
	}
	return resourceID, nil
}

// standingResource is the host's own machine id when its signed rules already let this peer start capability here (the
// server publishes one standing rule per capability the peer's account may use, until revoked; inspector only for the
// host's own account). A terminal, exec or inspector request
// then needs no per-operation scope, so no host refresh and no server request per stream; the credential was verified
// above, and a withdrawn rule closes the stream through its scope lease like any other. Revoking one access session
// therefore no longer ends a running stream by itself: the server withdraws the rule when it revokes the user's access.
func (s *Session) standingResource(capability string) string {
	if capability != "terminal" && capability != "exec" && capability != "inspector" {
		return ""
	}
	self, err := s.owner.authority.Self()
	if err != nil || !s.owner.authority.Allows(s.peerID, self.MachineID, capability, "accept") {
		return ""
	}
	return self.MachineID
}

// LimitConnection applies the same authenticated per-direction byte budget to
// a private application dispatch hop as to its incoming native stream.
func LimitConnection(connection net.Conn, maximum uint64) net.Conn {
	return newLimitedConn(connection, maximum)
}
