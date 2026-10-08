package runtime

import (
	"context"
	"crypto/tls"
	"errors"
	"github.com/pinksaucepasta/paperboat-tunnel/internal/reporting"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
)

type HTTP3Server struct {
	Reporter *reporting.Reporter
	address  string
	server   *http3.Server
	mu       sync.Mutex
	packet   net.PacketConn
	done     chan error
	closed   bool
}

func NewHTTP3Server(address string, handler http.Handler, tlsConfig *tls.Config, maxHeaderBytes int) (*HTTP3Server, error) {
	if _, _, err := net.SplitHostPort(address); err != nil || handler == nil || tlsConfig == nil || maxHeaderBytes < 1024 {
		return nil, ErrHTTPServerInvalid
	}
	server := &http3.Server{Addr: address, Handler: handler, TLSConfig: tlsConfig.Clone(), MaxHeaderBytes: maxHeaderBytes, IdleTimeout: 2 * time.Minute, QUICConfig: &quic.Config{Allow0RTT: false, HandshakeIdleTimeout: 10 * time.Second, MaxIdleTimeout: 2 * time.Minute, MaxIncomingStreams: 256, Versions: []quic.Version{quic.Version1, quic.Version2}}}
	return &HTTP3Server{address: address, server: server}, nil
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
	packet, err := net.ListenPacket("udp", s.address)
	if err != nil {
		return err
	}
	s.server.Handler = httpDiagnosticHandler(ctx, s.Reporter, s.server.Handler)
	s.server.Logger = slog.New(slog.NewTextHandler(httpDiagnosticWriter{ctx: ctx, reporter: s.Reporter}, nil))
	s.packet, s.done = packet, make(chan error, 1)
	go func() {
		err := s.server.Serve(packet)
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
	server, packet, done := s.server, s.packet, s.done
	s.mu.Unlock()
	if packet == nil {
		return nil
	}
	err := server.Shutdown(ctx)
	if err != nil {
		_ = server.Close()
	}
	_ = packet.Close()
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
