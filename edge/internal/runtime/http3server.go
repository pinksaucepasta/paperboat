package runtime

import (
	"context"
	"crypto/tls"
	"errors"
	"github.com/pinksaucepasta/paperboat/shared/reporting"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"

	"github.com/pinksaucepasta/paperboat/edge/internal/handover"
	"github.com/pinksaucepasta/paperboat/edge/internal/quicgate"
)

// visitorPendingHandshakes is how many unfinished QUIC handshakes one visitor address may hold (a per-client bound, not a node cap).
const visitorPendingHandshakes = 16

type HTTP3Server struct {
	Reporter  *reporting.Reporter
	address   string
	server    *http3.Server
	mu        sync.Mutex
	packet    net.PacketConn
	transport *quic.Transport
	done      chan error
	closed    bool
	// stopped is closed by Shutdown; it ends a wait for UDPReady.
	stopped chan struct{}
}

func NewHTTP3Server(address string, handler http.Handler, tlsConfig *tls.Config, maxHeaderBytes int) (*HTTP3Server, error) {
	if _, _, err := net.SplitHostPort(address); err != nil || handler == nil || tlsConfig == nil || maxHeaderBytes < 1024 {
		return nil, ErrHTTPServerInvalid
	}
	server := &http3.Server{Addr: address, Handler: handler, TLSConfig: tlsConfig.Clone(), MaxHeaderBytes: maxHeaderBytes, IdleTimeout: 2 * time.Minute, QUICConfig: &quic.Config{Allow0RTT: false, HandshakeIdleTimeout: 10 * time.Second, MaxIdleTimeout: 2 * time.Minute, MaxIncomingStreams: 256, Versions: []quic.Version{quic.Version1, quic.Version2}}}
	return &HTTP3Server{address: address, server: server, stopped: make(chan struct{})}, nil
}

func (s *HTTP3Server) Start(ctx context.Context) error {
	if ctx == nil {
		return ErrHTTPServerInvalid
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.packet != nil {
		return ErrHTTPServerInvalid
	}
	packet, err := handover.ListenPacket("udp", s.address)
	if err != nil {
		return err
	}
	s.server.Handler = httpDiagnosticHandler(ctx, s.Reporter, s.server.Handler)
	s.server.Logger = slog.New(slog.NewTextHandler(httpDiagnosticWriter{ctx: ctx, reporter: s.Reporter}, nil))
	// Visitors: a fresh address pays no Retry round trip; one with an unfinished handshake must prove itself, and no
	// address holds more than visitorPendingHandshakes unfinished (quicgate).
	s.server.ConnContext = func(ctx context.Context, c *quic.Conn) context.Context {
		go func() {
			select {
			case <-c.HandshakeComplete():
			case <-c.Context().Done():
			}
			quicgate.Settle(ctx)
		}()
		return ctx
	}
	transport := quicgate.NewTransport(packet, visitorPendingHandshakes, false)
	s.packet, s.transport, s.done = packet, transport, make(chan error, 1)
	stopped := s.stopped
	go func() {
		// An inheriting process reads UDP only after the process it replaces has exited.
		select {
		case <-handover.UDPReady():
		case <-stopped:
			s.done <- nil
			close(s.done)
			return
		}
		// ConfigureTLSConfig and an early listener are what http3.Server.Serve itself would set up.
		listener, err := transport.ListenEarly(http3.ConfigureTLSConfig(s.server.TLSConfig), s.server.QUICConfig)
		if err == nil {
			err = s.server.ServeListener(listener)
		}
		if errors.Is(err, net.ErrClosed) {
			err = nil
		}
		s.done <- err
		close(s.done)
	}()
	return nil
}

func (s *HTTP3Server) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	close(s.stopped)
	server, packet, transport, done := s.server, s.packet, s.transport, s.done
	s.mu.Unlock()
	if packet == nil {
		return nil
	}
	err := server.Shutdown(ctx)
	if err != nil {
		_ = server.Close()
	}
	_ = packet.Close()
	if transport != nil {
		_ = transport.Close()
	}
	select {
	case serveErr := <-done:
		return errors.Join(err, serveErr)
	case <-ctx.Done():
		return errors.Join(err, ctx.Err())
	}
}

func (s *HTTP3Server) Done() <-chan error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.done == nil {
		return closedErrorChannel(ErrHTTPServerInvalid)
	}
	return s.done
}
