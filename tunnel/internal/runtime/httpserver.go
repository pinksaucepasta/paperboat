package runtime

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"
)

var ErrHTTPServerInvalid = errors.New("private HTTP server configuration is invalid")

type HTTPServerSpec struct {
	Address           string
	Handler           http.Handler
	ReadHeaderTimeout time.Duration
	IdleTimeout       time.Duration
	MaxHeaderBytes    int
	TLSConfig         *tls.Config
	WrapListener      func(net.Listener) (net.Listener, error)
	Role              HTTPServerRole
}

type HTTPServerRole uint8

const (
	HTTPServerPrivate HTTPServerRole = iota
	HTTPServerPublicTLS
	HTTPServerPublicRedirect
)

type HTTPServer struct {
	spec     HTTPServerSpec
	mu       sync.Mutex
	server   *http.Server
	listener net.Listener
	done     chan error
	listen   func(string, string) (net.Listener, error)
	closed   bool
}

func NewHTTPServer(spec HTTPServerSpec) (*HTTPServer, error) {
	if err := validateHTTPServerSpec(spec); err != nil {
		return nil, err
	}
	return &HTTPServer{spec: spec, listen: net.Listen}, nil
}

func validateHTTPServerSpec(spec HTTPServerSpec) error {
	host, port, err := net.SplitHostPort(spec.Address)
	ip := net.ParseIP(host)
	roleValid := spec.Role == HTTPServerPrivate || spec.Role == HTTPServerPublicTLS || spec.Role == HTTPServerPublicRedirect
	transportValid := spec.Role == HTTPServerPublicRedirect && spec.TLSConfig == nil || spec.Role == HTTPServerPublicTLS && spec.TLSConfig != nil || spec.Role == HTTPServerPrivate && ip != nil && ip.IsLoopback()
	if err != nil || ip == nil || !roleValid || !transportValid || spec.Handler == nil || spec.ReadHeaderTimeout <= 0 || spec.IdleTimeout <= 0 || spec.MaxHeaderBytes < 1024 {
		return ErrHTTPServerInvalid
	}
	number, err := strconv.Atoi(port)
	if err != nil || number < 1 || number > 65535 {
		return ErrHTTPServerInvalid
	}
	return nil
}

func (s *HTTPServer) Start(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.server != nil || s.closed {
		return ErrHTTPServerInvalid
	}
	listener, err := s.listen("tcp", s.spec.Address)
	if err != nil {
		return err
	}
	if s.spec.WrapListener != nil {
		wrapped, wrapErr := s.spec.WrapListener(listener)
		if wrapErr != nil {
			_ = listener.Close()
			return wrapErr
		}
		listener = wrapped
	}
	s.listener = listener
	s.server = &http.Server{Handler: s.spec.Handler, ReadHeaderTimeout: s.spec.ReadHeaderTimeout, IdleTimeout: s.spec.IdleTimeout, MaxHeaderBytes: s.spec.MaxHeaderBytes, TLSConfig: s.spec.TLSConfig}
	s.done = make(chan error, 1)
	go func() {
		var err error
		if s.spec.TLSConfig != nil {
			err = s.server.ServeTLS(listener, "", "")
		} else {
			err = s.server.Serve(listener)
		}
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		s.done <- err
		close(s.done)
	}()
	return nil
}

func (s *HTTPServer) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	server, done := s.server, s.done
	s.mu.Unlock()
	if server == nil {
		return nil
	}
	shutdownErr := server.Shutdown(ctx)
	if shutdownErr != nil {
		_ = server.Close()
	}
	select {
	case serveErr := <-done:
		return errors.Join(shutdownErr, serveErr)
	case <-ctx.Done():
		_ = server.Close()
		return errors.Join(shutdownErr, ctx.Err())
	}
}

func (s *HTTPServer) Done() <-chan error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.done == nil {
		return closedErrorChannel(ErrHTTPServerInvalid)
	}
	return s.done
}
