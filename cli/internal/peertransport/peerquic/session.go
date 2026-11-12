// Package peerquic owns native Paperboat QUIC over authorized packet sockets.
package peerquic

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/diagnostics"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/fixedpacket"
	"github.com/pinksaucepasta/paperboat/shared/diagnosticlog"
	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/qlog"
	"github.com/quic-go/quic-go/qlogwriter"
)

const (
	ALPN             = "paperboat-peer-v1"
	PrivateHTTP3ALPN = "paperboat-private-http-v1"
)

type Class uint8

const (
	ClassInteractive Class = iota + 1
	ClassPreview
	ClassTransfer
)

type SessionConfig struct {
	Class             Class
	AcceptsHTTP3      bool
	KeepAlivePeriod   time.Duration
	MaxIdleTimeout    time.Duration
	InitialPacketSize uint16
	// HandshakeTimeout is how long a dial waits for the handshake to complete before it gives up (0: one second,
	// the most any dial waits). A dial right after the peer restarted uses a short one: its first flight may meet
	// an engine that is not answering yet, and the retry that follows succeeds in milliseconds.
	HandshakeTimeout time.Duration
}

func DevelopmentSessionConfig(class Class) SessionConfig {
	return SessionConfig{
		Class: class, KeepAlivePeriod: 3 * time.Second,
		MaxIdleTimeout: 2 * time.Minute, InitialPacketSize: 1200,
	}
}

func (c SessionConfig) WithKeepAlive(period time.Duration) (SessionConfig, error) {
	c.KeepAlivePeriod = period
	if err := c.validate(); err != nil {
		return SessionConfig{}, err
	}
	return c, nil
}

func (c SessionConfig) WithInitialPacketSize(size uint16) (SessionConfig, error) {
	c.InitialPacketSize = size
	if err := c.validate(); err != nil {
		return SessionConfig{}, err
	}
	return c, nil
}

func (c SessionConfig) validate() error {
	if !validClass(c.Class) || c.KeepAlivePeriod <= 0 || c.MaxIdleTimeout <= 2*c.KeepAlivePeriod || c.InitialPacketSize < 1200 || c.HandshakeTimeout < 0 {
		return errors.New("invalid peer QUIC session configuration")
	}
	return nil
}

type Session struct {
	Connection *quic.Conn
	transport  *quic.Transport
	pto        *ptoTrace
	closeOnce  sync.Once
	closeErr   error
}

func Dial(ctx context.Context, iceConn net.Conn, tlsConfig *tls.Config, class Class) (*Session, error) {
	return DialConfigured(ctx, iceConn, tlsConfig, DevelopmentSessionConfig(class))
}

func DialConfigured(ctx context.Context, iceConn net.Conn, tlsConfig *tls.Config, sessionConfig SessionConfig) (*Session, error) {
	if err := sessionConfig.validate(); err != nil {
		return nil, err
	}
	if err := validateClientTLS(tlsConfig); err != nil {
		return nil, err
	}
	return dial(ctx, iceConn, tlsConfig, quicConfig(sessionConfig))
}

// DialProbe creates a probe-only connection with automatic keepalives disabled
// so an idle interval measures the network mapping rather than QUIC traffic.
func DialProbe(ctx context.Context, iceConn net.Conn, tlsConfig *tls.Config, maximumIdle time.Duration) (*Session, error) {
	if err := validateClientTLS(tlsConfig); err != nil || maximumIdle <= 0 {
		if err == nil {
			err = errors.New("probe maximum idle must be positive")
		}
		return nil, err
	}
	return dial(ctx, iceConn, tlsConfig, probeConfig(maximumIdle))
}

func dial(ctx context.Context, iceConn net.Conn, tlsConfig *tls.Config, quicConfig *quic.Config) (*Session, error) {
	packetConn, err := fixedpacket.New(iceConn)
	if err != nil {
		return nil, err
	}
	return dialPacket(ctx, packetConn, packetConn.RemoteAddr(), tlsConfig, quicConfig)
}

func dialPacket(ctx context.Context, packetConn net.PacketConn, remote net.Addr, tlsConfig *tls.Config, quicConfig *quic.Config) (*Session, error) {
	transport := &quic.Transport{Conn: packetConn, ConnectionIDLength: 8, DisableVersionNegotiationPackets: true}
	pto := newPTOTrace()
	diagnostics.ReconnectEvent(ctx, "quic_trace", pto.id, nil)
	// ET ClientConnection.cpp:pollReconnect retries fresh sockets each second.
	// quic-go connection.go:695 bounds silence only before handshake completion.
	configured := quicConfig.Clone()
	if configured.HandshakeIdleTimeout <= 0 || configured.HandshakeIdleTimeout > time.Second {
		configured.HandshakeIdleTimeout = time.Second
	}
	configured.Tracer = func(_ context.Context, _ bool, id quic.ConnectionID) qlogwriter.Trace {
		return &wireTrace{pto: pto, id: id.String()}
	}
	connection, err := transport.Dial(ctx, remote, tlsConfig.Clone(), configured)
	if err != nil {
		_ = transport.Close()
		return nil, fmt.Errorf("dial peer QUIC: %w", err)
	}
	return &Session{Connection: connection, transport: transport, pto: pto}, nil
}

type Listener struct {
	listener  *quic.Listener
	transport *quic.Transport
	pto       *ptoTrace
	closeOnce sync.Once
	closeErr  error
	mu        sync.Mutex
	closed    bool
	accepted  map[*Session]struct{}
	// closingUntil is the latest ended connection's closing period; guarded by mu.
	closingUntil time.Time
}

func Listen(iceConn net.Conn, tlsConfig *tls.Config, class Class) (*Listener, error) {
	return ListenConfigured(iceConn, tlsConfig, DevelopmentSessionConfig(class))
}

func ListenConfigured(iceConn net.Conn, tlsConfig *tls.Config, sessionConfig SessionConfig) (*Listener, error) {
	if err := sessionConfig.validate(); err != nil {
		return nil, err
	}
	if err := validateServerTLS(tlsConfig); err != nil {
		return nil, err
	}
	return listen(iceConn, tlsConfig, quicConfig(sessionConfig))
}

// ListenProbe accepts only probe-control streams and sends no automatic
// keepalive packets during the measured idle period.
func ListenProbe(iceConn net.Conn, tlsConfig *tls.Config, maximumIdle time.Duration) (*Listener, error) {
	if err := validateServerTLS(tlsConfig); err != nil || maximumIdle <= 0 {
		if err == nil {
			err = errors.New("probe maximum idle must be positive")
		}
		return nil, err
	}
	return listen(iceConn, tlsConfig, probeConfig(maximumIdle))
}

func listen(iceConn net.Conn, tlsConfig *tls.Config, quicConfig *quic.Config) (*Listener, error) {
	started := time.Now()
	timing := map[string]int64{}
	defer func() {
		diagnosticlog.TryInfo("peer QUIC listener timing", "milestones_us", timing, "elapsed_us", time.Since(started).Microseconds())
	}()
	packetConn, err := fixedpacket.New(iceConn)
	if err != nil {
		return nil, err
	}
	timing["packet_adapter_ready"] = time.Since(started).Microseconds()
	listener, err := listenPacket(packetConn, tlsConfig, quicConfig)
	timing["transport_listen_returned"] = time.Since(started).Microseconds()
	return listener, err
}

func listenPacket(packetConn net.PacketConn, tlsConfig *tls.Config, quicConfig *quic.Config) (*Listener, error) {
	// Reuse the endpoint's private identity across carrier processes. A new
	// transport can then authenticate a reset for a connection ID it no longer
	// owns, instead of leaving the peer waiting for PTO or the idle timeout.
	encoded, err := x509.MarshalPKCS8PrivateKey(tlsConfig.Certificates[0].PrivateKey)
	if err != nil {
		return nil, fmt.Errorf("derive peer QUIC reset identity: %w", err)
	}
	defer clear(encoded)
	resetKey := new(quic.StatelessResetKey)
	mac := hmac.New(sha256.New, encoded)
	_, _ = mac.Write([]byte("paperboat/peer-quic/stateless-reset/v1"))
	copy(resetKey[:], mac.Sum(nil))
	transport := &quic.Transport{Conn: packetConn, ConnectionIDLength: 8, DisableVersionNegotiationPackets: true, StatelessResetKey: resetKey}
	pto := newPTOTrace()
	configured := quicConfig.Clone()
	configured.Tracer = func(_ context.Context, _ bool, id quic.ConnectionID) qlogwriter.Trace {
		return &wireTrace{pto: pto, id: id.String()}
	}
	listener, err := transport.Listen(tlsConfig.Clone(), configured)
	if err != nil {
		_ = transport.Close()
		return nil, fmt.Errorf("listen peer QUIC: %w", err)
	}
	return &Listener{listener: listener, transport: transport, pto: pto}, nil
}

func probeConfig(maximumIdle time.Duration) *quic.Config {
	probeSession := DevelopmentSessionConfig(ClassInteractive)
	probeSession.MaxIdleTimeout = maximumIdle
	result := quicConfig(probeSession)
	result.KeepAlivePeriod = 0
	result.MaxIncomingStreams = 3
	return result
}

func (l *Listener) Accept(ctx context.Context) (*Session, error) {
	connection, err := l.listener.Accept(ctx)
	if err != nil {
		return nil, fmt.Errorf("accept peer QUIC: %w", err)
	}
	session := &Session{Connection: connection, pto: l.pto}
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		_ = session.Close()
		return nil, net.ErrClosed
	}
	if l.accepted == nil {
		l.accepted = make(map[*Session]struct{})
	}
	l.accepted[session] = struct{}{}
	l.mu.Unlock()
	context.AfterFunc(connection.Context(), func() {
		until := time.Now().Add(closingPeriod(connection))
		l.mu.Lock()
		delete(l.accepted, session)
		if until.After(l.closingUntil) {
			l.closingUntil = until
		}
		l.mu.Unlock()
	})
	return session, nil
}

func (l *Listener) Close() error {
	if l == nil {
		return nil
	}
	l.closeOnce.Do(func() {
		l.mu.Lock()
		l.closed = true
		sessions := make([]*Session, 0, len(l.accepted))
		for session := range l.accepted {
			sessions = append(sessions, session)
		}
		l.mu.Unlock()
		// Stop admission, then send every authenticated application close
		// before destroying the shared UDP transport. Closing its socket first
		// leaves an established peer waiting for PTO/idle loss detection.
		listenerErr := l.listener.Close()
		var closing sync.WaitGroup
		failures := make([]error, len(sessions))
		for index, session := range sessions {
			closing.Add(1)
			go func() { defer closing.Done(); failures[index] = session.Close() }()
		}
		closing.Wait()
		l.mu.Lock()
		for _, session := range sessions {
			if next := time.Now().Add(closingPeriod(session.Connection)); next.After(l.closingUntil) {
				l.closingUntil = next
			}
		}
		l.mu.Unlock()
		diagnosticlog.TryInfo("peer QUIC close", "trace_id", l.pto.id, "stage", "listener_packet_close")
		l.closeErr = errors.Join(listenerErr, errors.Join(failures...), l.transport.Conn.Close(), l.transport.Close())
	})
	return l.closeErr
}

// ClosingDeadline is when the latest locally closed connection's closing
// period ends. Closing the socket keeps a CONNECTION_CLOSE already queued in a
// userspace path; tearing that network down before this deadline drops it.
func (l *Listener) ClosingDeadline() time.Time {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.closingUntil
}

func (s *Session) Close() error {
	if s == nil || s.Connection == nil {
		return nil
	}
	s.closeOnce.Do(func() {
		diagnosticlog.TryInfo("peer QUIC close", "trace_id", s.pto.id, "stage", "application_close_start")
		var packetErr, transportErr error
		// Send QUIC's authenticated close while the packet transport is still
		// writable. Closing the ICE/UDP socket first strands the peer until its
		// idle timeout, which leaks host-side carriers after the final lease.
		// quic-go's CloseWithError waits for the connection context to finish.
		// Initiate it asynchronously and bound the flush wait so carrier cleanup
		// cannot stall on a peer that is already unreachable.
		connectionDone := make(chan error, 1)
		go func() { connectionDone <- s.Connection.CloseWithError(0, "") }()
		var connectionErr error
		select {
		case connectionErr = <-connectionDone:
			diagnosticlog.TryInfo("peer QUIC close", "trace_id", s.pto.id, "stage", "application_close_return", "failed", connectionErr != nil)
		case <-time.After(250 * time.Millisecond):
			diagnosticlog.TryInfo("peer QUIC close", "trace_id", s.pto.id, "stage", "application_close_timeout")
			// Transport.Close below forcefully completes the QUIC connection;
			// timeout here is an expected bounded-cleanup outcome, not an
			// application failure.
			connectionErr = nil
		}
		if s.transport != nil {
			diagnosticlog.TryInfo("peer QUIC close", "trace_id", s.pto.id, "stage", "packet_transport_close")
			transportErr = s.transport.Close()
			packetErr = s.transport.Conn.Close()
		}
		s.closeErr = errors.Join(connectionErr, packetErr, transportErr)
	})
	return s.closeErr
}

// closingPeriod is RFC 9000 section 10.2's closing state, three PTOs (quic-go
// keeps its closed-connection handler as long: connection.go ReplaceWithClosed).
// CloseWithError only queues the CONNECTION_CLOSE in a userspace path
// (netstack, then WireGuard); a carrier shutdown closing that network at once
// dropped it under CPU load and left the peer waiting for its idle timeout.
// Closing only the socket keeps the queued frame, so no Close waits: the
// carrier waits for Listener.ClosingDeadline before its network closes, and
// socket flow accounting frees at once (RetireIdleClient). Nothing was sent
// after a remote close or a timeout.
func closingPeriod(connection *quic.Conn) time.Duration {
	var application *quic.ApplicationError
	var transport *quic.TransportError
	cause := context.Cause(connection.Context())
	if !(errors.As(cause, &application) && !application.Remote) && !(errors.As(cause, &transport) && !transport.Remote) {
		return 0
	}
	stats := connection.ConnectionStats()
	// RFC 9002 section 6.2.1; 25 ms is the default max_ack_delay quic-go uses.
	pto := stats.SmoothedRTT + max(4*stats.MeanDeviation, time.Millisecond) + 25*time.Millisecond
	return min(3*pto, time.Second)
}

// ClosingPeriod is how long a network carrying this locally closed session
// must stay up for its queued CONNECTION_CLOSE; zero when nothing was sent.
func (s *Session) ClosingPeriod() time.Duration {
	if s == nil || s.Connection == nil {
		return 0
	}
	return closingPeriod(s.Connection)
}

func (s *Session) PTOCount() uint32 {
	if s == nil || s.pto == nil {
		return 0
	}
	return s.pto.total.Load()
}

// PTOChanged reports RFC 9002 probe-timeout progress without polling. The
// notification is edge-triggered; PTOCount is the authoritative total.
func (s *Session) PTOChanged() <-chan struct{} {
	if s == nil || s.pto == nil {
		return nil
	}
	return s.pto.changed
}

type ptoTrace struct {
	id      string
	current atomic.Uint32
	total   atomic.Uint32
	changed chan struct{}
}

func newPTOTrace() *ptoTrace { return &ptoTrace{id: rand.Text(), changed: make(chan struct{}, 1)} }

func (*ptoTrace) SupportsSchemas(schema string) bool { return schema == qlog.EventSchema }
func (t *ptoTrace) AddProducer() qlogwriter.Recorder { return &ptoRecorder{ptoTrace: t} }

type ptoRecorder struct {
	*ptoTrace
	connection string
}

func (r *ptoRecorder) RecordEvent(event qlogwriter.Event) {
	r.recordWireEvent(event)
	updated, ok := event.(qlog.PTOCountUpdated)
	if !ok {
		if pointer, pointerOK := event.(*qlog.PTOCountUpdated); pointerOK {
			updated, ok = *pointer, true
		}
	}
	if !ok {
		return
	}
	previous := r.current.Swap(updated.PTOCount)
	if updated.PTOCount > previous {
		r.total.Add(updated.PTOCount - previous)
		if r.changed != nil {
			select {
			case r.changed <- struct{}{}:
			default:
			}
		}
	}
}

func (*ptoRecorder) Close() error { return nil }

func config(class Class) *quic.Config {
	return quicConfig(DevelopmentSessionConfig(class))
}

// maxInteractiveStreams is how many streams one peer may have open on an interactive session. A terminal holds two (control
// and output) and a machine holds 100 terminals plus the viewers of a shared one (F21-164): the previous 64 stopped a client
// at 32 terminals, the rest waiting out their credential. It is only stream credit; the connection window still bounds memory.
const maxInteractiveStreams = 512

func quicConfig(sessionConfig SessionConfig) *quic.Config {
	maximumStreams := int64(64)
	maximumUniStreams := int64(-1)
	if sessionConfig.AcceptsHTTP3 {
		maximumUniStreams = 16
	}
	switch sessionConfig.Class {
	case ClassPreview:
		maximumStreams = 128
		// HTTP/3 owns control and QPACK unidirectional streams after
		// authenticated preview admission.
		maximumUniStreams = 16
	case ClassTransfer:
		maximumStreams = 16
	case ClassInteractive:
		maximumStreams = maxInteractiveStreams
	default:
		maximumStreams = -1
	}
	return &quic.Config{
		HandshakeIdleTimeout:           handshakeTimeout(sessionConfig),
		MaxIdleTimeout:                 sessionConfig.MaxIdleTimeout,
		KeepAlivePeriod:                sessionConfig.KeepAlivePeriod,
		InitialPacketSize:              sessionConfig.InitialPacketSize,
		DisablePathMTUDiscovery:        false,
		MaxIncomingStreams:             maximumStreams,
		MaxIncomingUniStreams:          maximumUniStreams,
		Allow0RTT:                      false,
		EnableDatagrams:                false,
		InitialStreamReceiveWindow:     256 << 10,
		MaxStreamReceiveWindow:         4 << 20,
		InitialConnectionReceiveWindow: 512 << 10,
		MaxConnectionReceiveWindow:     16 << 20,
	}
}

// handshakeTimeout is the session's own bound, or the long one that dialPacket clamps to one second.
func handshakeTimeout(c SessionConfig) time.Duration {
	if c.HandshakeTimeout > 0 {
		return c.HandshakeTimeout
	}
	return 10 * time.Second
}

func validClass(class Class) bool {
	return class == ClassInteractive || class == ClassPreview || class == ClassTransfer
}

func validateClientTLS(config *tls.Config) error {
	if err := validateCommonTLS(config); err != nil {
		return err
	}
	if !config.InsecureSkipVerify || config.VerifyConnection == nil || config.RootCAs != nil || len(config.Certificates) != 1 {
		return errors.New("peer QUIC client requires endpoint-certificate verification")
	}
	return nil
}

func validateServerTLS(config *tls.Config) error {
	if err := validateCommonTLS(config); err != nil {
		return err
	}
	if !config.InsecureSkipVerify || config.VerifyConnection == nil || len(config.Certificates) != 1 || config.ClientCAs != nil || config.ClientAuth != tls.RequireAnyClientCert {
		return errors.New("peer QUIC server requires endpoint-certificate verification")
	}
	return nil
}

func validateCommonTLS(config *tls.Config) error {
	if config == nil || config.MinVersion != tls.VersionTLS13 || config.MaxVersion != 0 && config.MaxVersion != tls.VersionTLS13 {
		return errors.New("peer QUIC requires TLS 1.3")
	}
	validALPN := len(config.NextProtos) == 1 && (config.NextProtos[0] == ALPN || config.NextProtos[0] == PrivateHTTP3ALPN) || len(config.NextProtos) == 2 && config.NextProtos[0] == ALPN && config.NextProtos[1] == PrivateHTTP3ALPN
	if !validALPN {
		return errors.New("peer QUIC requires the Paperboat v1 ALPN")
	}
	return nil
}
