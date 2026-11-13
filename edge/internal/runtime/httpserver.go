package runtime

import (
	"context"
	"crypto/tls"
	"errors"
	"github.com/pinksaucepasta/paperboat/shared/reporting"
	"net"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pinksaucepasta/paperboat/edge/internal/handover"
)

var ErrHTTPServerInvalid = errors.New("private HTTP server configuration is invalid")

type HTTPServerSpec struct {
	Reporter          *reporting.Reporter
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
	drainer  Drainer
	draining bool // Drain closed the listener itself: the serve loop ending on it is not a failure
}

func NewHTTPServer(spec HTTPServerSpec) (*HTTPServer, error) {
	if err := validateHTTPServerSpec(spec); err != nil {
		return nil, err
	}
	return &HTTPServer{spec: spec, listen: handover.Listen}, nil
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

func (s *HTTPServer) Start(ctx context.Context) error {
	if ctx == nil {
		return ErrHTTPServerInvalid
	}
	if err := ctx.Err(); err != nil {
		return err
	}
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
	s.server = &http.Server{Handler: s.drainer.Wrap(httpDiagnosticHandler(ctx, s.spec.Reporter, s.spec.Handler)), ErrorLog: httpDiagnosticLogger(ctx, s.spec.Reporter), ReadHeaderTimeout: s.spec.ReadHeaderTimeout, IdleTimeout: s.spec.IdleTimeout, MaxHeaderBytes: s.spec.MaxHeaderBytes, TLSConfig: s.spec.TLSConfig}
	s.done = make(chan error, 1)
	go func() {
		var err error
		if s.spec.TLSConfig != nil {
			err = s.server.ServeTLS(listener, "", "")
		} else {
			err = s.server.Serve(listener)
		}
		s.mu.Lock()
		draining := s.draining
		s.mu.Unlock()
		if errors.Is(err, http.ErrServerClosed) || draining && errors.Is(err, net.ErrClosed) {
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

// httpDrainGrace is how long a draining public server keeps answering requests on connections its clients already
// hold (each answer says Connection: close) before it closes whatever is left idle.
const httpDrainGrace = 3 * time.Second

// Drainer is the graceful stop of a public HTTP server during an edge handover, the drain Envoy and HAProxy use: stop
// accepting (the replacement process owns the accept queue now), make every response from then on end its connection
// (Connection: close, through Wrap), give clients a bounded grace to move off the connections they keep alive, then
// Shutdown. A plain Shutdown closes idle keep-alive connections at once, so a client reusing one at that instant got a
// bare EOF; now it gets its answer and a connection close to follow. (Server.SetKeepAlivesEnabled(false) is no help: it
// closes the idle connections itself.)
type Drainer struct{ draining atomic.Bool }

// Wrap marks every response as the last one on its connection once the drain has begun.
func (d *Drainer) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if d.draining.Load() {
			w.Header().Set("Connection", "close")
		}
		next.ServeHTTP(w, r)
	})
}

// Drain runs the drain on server (whose handler went through Wrap) and its listener.
func (d *Drainer) Drain(ctx context.Context, server *http.Server, listener net.Listener, grace time.Duration) error {
	d.draining.Store(true)
	if listener != nil {
		_ = listener.Close()
	}
	if until, ok := ctx.Deadline(); ok {
		grace = min(grace, time.Until(until)/2) // the rest of the bound is for the requests still in flight
	}
	if grace > 0 {
		select {
		case <-time.After(grace):
		case <-ctx.Done():
		}
	}
	return server.Shutdown(ctx)
}

// Drain stops this server for a handover (Drainer.Drain), then waits for the serve loop as Shutdown does.
func (s *HTTPServer) Drain(ctx context.Context) error {
	s.mu.Lock()
	if s.closed || s.server == nil {
		s.mu.Unlock()
		return s.Shutdown(ctx)
	}
	s.draining = true
	server, listener := s.server, s.listener
	s.mu.Unlock()
	drainErr := s.drainer.Drain(ctx, server, listener, httpDrainGrace)
	return errors.Join(drainErr, s.Shutdown(ctx))
}

func (s *HTTPServer) Done() <-chan error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.done == nil {
		return closedErrorChannel(ErrHTTPServerInvalid)
	}
	return s.done
}
