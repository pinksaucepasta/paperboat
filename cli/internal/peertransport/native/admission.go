package native

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/diagnostics"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/streamauth"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/tailnet"
	"github.com/pinksaucepasta/paperboat/shared/reattach"
	"github.com/pinksaucepasta/paperboat/shared/serverclock"
	"github.com/quic-go/quic-go"
)

// Like EternalTerminal ServerConnection.cpp RETRY_LATER, refuse admission
// explicitly. QUIC stream resets preserve the established peer connection.
const (
	AdmissionDenied      quic.StreamErrorCode = 0x5043
	AdmissionUnavailable quic.StreamErrorCode = 0x5044
)

type AdmissionRefusal struct {
	Code quic.StreamErrorCode
	Err  error
}

func (e *AdmissionRefusal) Error() string {
	if e.Code == AdmissionUnavailable {
		return "native application admission is temporarily unavailable"
	}
	return "native application admission was denied"
}

func (e *AdmissionRefusal) DiagnosticStatus() int {
	if e.Code == AdmissionUnavailable {
		return http.StatusServiceUnavailable
	}
	return http.StatusForbidden
}

func (e *AdmissionRefusal) Unwrap() error        { return e.Err }
func (e *AdmissionRefusal) Is(target error) bool { return target == tailnet.ErrAdmission }

// AdmissionRetryInterval bounds retries inside one application admission.
const AdmissionRetryInterval = 100 * time.Millisecond

// The success byte keeps application bytes behind admission. A refusal uses
// QUIC's typed stream reset; retrying it cannot replay an application request.
func (s *Session) openAdmitted(
	ctx context.Context, header streamauth.Header, encoded []byte,
	resource, capability string,
) (net.Conn, error) {
	deadline := time.Now().Add(serverclock.Until(time.Unix(header.DeadlineUnix, 0))) // local timer, server deadline (F21-02)
	if header.Consumer == "exec" {
		return s.openEarly(ctx, encoded, resource, capability, deadline, header)
	}
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	connection, err := s.admit(ctx, nil, encoded, resource, capability, nil)
	if err != nil {
		return nil, err
	}
	return s.bindScope(newLimitedConn(connection, header.MaximumBytes), resource, capability, "dial", time.Unix(header.DeadlineUnix, 0))
}

// admit waits for the host's admission of connection (nil: open one), retrying a temporary refusal on a new stream
// until ctx ends. replay rewrites what the caller had already sent on the refused stream, which the host never read.
func (s *Session) admit(
	ctx context.Context, connection net.Conn, encoded []byte, resource, capability string, replay func(net.Conn) error,
) (net.Conn, error) {
	for {
		if connection == nil {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			var err error
			if connection, err = s.openHeader(ctx, encoded, resource, capability); err != nil {
				return nil, err
			}
			if replay != nil {
				if err := replay(connection); err != nil {
					_ = connection.Close()
					return nil, err
				}
			}
		}
		end := diagnostics.ReconnectStep(ctx, "admission_ack")
		err := readAdmissionAck(ctx, connection)
		var reset *quic.StreamError
		if errors.As(err, &reset) && reset.Remote && (reset.ErrorCode == AdmissionDenied || reset.ErrorCode == AdmissionUnavailable) {
			err = &AdmissionRefusal{Code: reset.ErrorCode, Err: err}
		}
		end(err)
		if err == nil {
			return connection, nil
		}
		if stream, ok := connection.(interface{ CancelRead(quic.StreamErrorCode) }); ok {
			stream.CancelRead(0)
		}
		_ = connection.Close()
		connection = nil
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		var refusal *AdmissionRefusal
		if !errors.As(err, &refusal) || refusal.Code != AdmissionUnavailable {
			return nil, err
		}
		if err := reattach.Wait(ctx, AdmissionRetryInterval); err != nil {
			return nil, err
		}
	}
}

// openHeader opens a stream and sends the credential header, while the network scope still admits the dial.
func (s *Session) openHeader(ctx context.Context, encoded []byte, resource, capability string) (net.Conn, error) {
	if !s.dialAdmits(resource, capability) {
		return nil, tailnet.ErrAdmission
	}
	connection, err := s.Open(ctx)
	if err != nil {
		return nil, err
	}
	if err := writeHeader(connection, encoded); err != nil {
		_ = connection.Close()
		return nil, err
	}
	return connection, nil
}

// openEarly returns an exec stream before the host admits it, so the client sends its preface, hello and start request
// in the same flight as the credential instead of one round trip each. The host reads nothing past the header until it
// admits the stream (a refusal resets it unread), and the first Read returns the refusal, so it fails as closed as
// waiting did.
func (s *Session) openEarly(
	ctx context.Context, encoded []byte, resource, capability string, deadline time.Time, header streamauth.Header,
) (net.Conn, error) {
	ctx, cancel := context.WithDeadline(ctx, deadline)
	connection, err := s.openHeader(ctx, encoded, resource, capability)
	cancel()
	if err != nil {
		return nil, err
	}
	early := &earlyConn{conn: connection}
	early.ctx, early.cancel = context.WithDeadline(context.Background(), deadline)
	early.admit = func(conn net.Conn, replay func(net.Conn) error) (net.Conn, error) {
		return s.admit(early.ctx, conn, encoded, resource, capability, replay)
	}
	return s.bindScope(newLimitedConn(early, header.MaximumBytes), resource, capability, "dial", time.Unix(header.DeadlineUnix, 0))
}

// earlyMaxPending bounds what is sent ahead of admission (kept for a replay); more waits for the admission.
const earlyMaxPending = 64 << 10

// earlyConn is a stream sent on before its admission: the first Read (or a Write past earlyMaxPending) waits for it.
type earlyConn struct {
	ctx    context.Context
	cancel context.CancelFunc
	admit  func(net.Conn, func(net.Conn) error) (net.Conn, error)

	resolveMu sync.Mutex // one admission wait at a time; later callers see its result

	mu                          sync.Mutex // conn, pending, writes before admission
	conn                        net.Conn
	pending                     []byte
	admitted, closed            bool
	err, writeErr               error
	readDeadline, writeDeadline time.Time
}

func (c *earlyConn) resolve() error {
	c.resolveMu.Lock()
	defer c.resolveMu.Unlock()
	c.mu.Lock()
	first, admitted, err := c.conn, c.admitted, c.err
	c.mu.Unlock()
	if admitted || err != nil {
		return err
	}
	conn, err := c.admit(first, func(next net.Conn) error {
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.closed {
			return net.ErrClosed
		}
		c.conn, c.writeErr = next, nil
		_ = next.SetWriteDeadline(c.writeDeadline)
		return writeFull(next, c.pending)
	})
	c.mu.Lock()
	defer c.mu.Unlock()
	if err == nil && c.writeErr != nil {
		err = c.writeErr // a write the host may not have received: never continue past a gap
		_ = conn.Close()
	}
	if err == nil {
		c.conn, c.admitted, c.pending = conn, true, nil
		_ = conn.SetReadDeadline(c.readDeadline)
		return nil
	}
	c.err = err
	return err
}

func (c *earlyConn) Read(p []byte) (int, error) {
	if err := c.resolve(); err != nil {
		return 0, err
	}
	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()
	return conn.Read(p)
}

func (c *earlyConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	if c.err != nil {
		c.mu.Unlock()
		return 0, c.err
	}
	if !c.admitted && len(c.pending)+len(p) <= earlyMaxPending {
		defer c.mu.Unlock()
		c.pending = append(c.pending, p...)
		// A refused stream's send side may be stopped: the admission decides, and a replay resends.
		if err := writeFull(c.conn, p); err != nil && c.writeErr == nil {
			c.writeErr = err
		}
		return len(p), nil
	}
	c.mu.Unlock()
	if err := c.resolve(); err != nil {
		return 0, err
	}
	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()
	return conn.Write(p)
}

func (c *earlyConn) Close() error {
	c.cancel()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	return c.conn.Close()
}

func (c *earlyConn) CloseWrite() error {
	if err := c.resolve(); err != nil {
		return err
	}
	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()
	if half, ok := conn.(interface{ CloseWrite() error }); ok {
		return half.CloseWrite()
	}
	return conn.Close()
}

func (c *earlyConn) LocalAddr() net.Addr  { return c.current().LocalAddr() }
func (c *earlyConn) RemoteAddr() net.Addr { return c.current().RemoteAddr() }
func (c *earlyConn) current() net.Conn {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conn
}

func (c *earlyConn) SetDeadline(t time.Time) error {
	return errors.Join(c.SetReadDeadline(t), c.SetWriteDeadline(t))
}

func (c *earlyConn) SetReadDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.readDeadline = t
	if !c.admitted { // the admission wait owns the read deadline until it ends
		return nil
	}
	return c.conn.SetReadDeadline(t)
}

func (c *earlyConn) SetWriteDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.writeDeadline = t
	return c.conn.SetWriteDeadline(t)
}

func readAdmissionAck(ctx context.Context, connection net.Conn) error {
	deadline, _ := ctx.Deadline()
	if err := connection.SetReadDeadline(deadline); err != nil {
		return err
	}
	done := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		_ = connection.SetReadDeadline(time.Now())
		close(done)
	})
	var ack [1]byte
	_, err := io.ReadFull(connection, ack[:])
	if !stop() {
		<-done
	}
	err = errors.Join(err, connection.SetReadDeadline(time.Time{}))
	if err == nil && ack[0] != 0 {
		return streamauth.ErrInvalid
	}
	return err
}

func acknowledgeAdmission(connection net.Conn) error {
	if err := connection.SetWriteDeadline(time.Now().Add(authorizationHeaderTimeout)); err != nil {
		return err
	}
	return errors.Join(writeFull(connection, []byte{0}), connection.SetWriteDeadline(time.Time{}))
}
