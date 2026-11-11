package runtime

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

var (
	ErrHTTPInvalid = errors.New("invalid runtime HTTP configuration")
	ErrHTTPState   = errors.New("invalid runtime HTTP state")
)

type ListenerFactory func() (net.Listener, error)

type HTTPConfig struct {
	Address           string
	Handler           http.Handler
	Listener          ListenerFactory
	ReadHeaderTimeout time.Duration
	IdleTimeout       time.Duration
	MaxHeaderBytes    int
	NativeHandler     func(net.Conn)
}

type HTTPService struct {
	mu       sync.Mutex
	config   HTTPConfig
	server   *http.Server
	listener net.Listener
	done     chan struct{}
	serveErr error
	running  bool
	stopped  bool
	retiring bool // StopAccepting closed the listener
	// connections counts accepted HTTP connections that have not closed.
	connections atomic.Int64
}

func NewHTTPService(config HTTPConfig) (*HTTPService, error) {
	if config.ReadHeaderTimeout == 0 {
		config.ReadHeaderTimeout = 5 * time.Second
	}
	if config.IdleTimeout == 0 {
		config.IdleTimeout = 60 * time.Second
	}
	if config.MaxHeaderBytes == 0 {
		config.MaxHeaderBytes = 32 << 10
	}
	if config.Handler == nil || config.ReadHeaderTimeout <= 0 || config.IdleTimeout <= 0 || config.MaxHeaderBytes < 1024 || config.MaxHeaderBytes > 64<<10 || config.Listener == nil && !LoopbackAddress(config.Address) {
		return nil, ErrHTTPInvalid
	}
	return &HTTPService{config: config}, nil
}

func (s *HTTPService) Start(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running || s.stopped {
		return ErrHTTPState
	}
	listener, err := s.listen(ctx)
	if err != nil {
		return err
	}
	s.server = &http.Server{Handler: s.config.Handler, ReadHeaderTimeout: s.config.ReadHeaderTimeout, IdleTimeout: s.config.IdleTimeout, MaxHeaderBytes: s.config.MaxHeaderBytes, ConnState: s.trackConnection}
	s.serveLocked(listener)
	return nil
}

// listenBusyWait bounds how long Start and ResumeAccepting wait for the address to come free: in a Windows hostd
// handover the previous generation's worker closes it a few milliseconds before this one binds.
const listenBusyWait = 2 * time.Second

func (s *HTTPService) listen(ctx context.Context) (net.Listener, error) {
	var listener net.Listener
	var err error
	if s.config.Listener != nil {
		listener, err = s.config.Listener()
	} else {
		deadline := time.Now().Add(listenBusyWait)
		for {
			listener, err = net.Listen("tcp", s.config.Address)
			if err == nil || !addressBusy(err) || time.Now().After(deadline) {
				break
			}
			select {
			case <-ctx.Done():
				return nil, errors.Join(err, ctx.Err())
			case <-time.After(20 * time.Millisecond):
			}
		}
	}
	if err != nil {
		return nil, err
	}
	if s.config.NativeHandler != nil {
		listener = &protocolListener{Listener: listener, native: s.config.NativeHandler, timeout: s.config.ReadHeaderTimeout}
	}
	return listener, nil
}

func (s *HTTPService) serveLocked(listener net.Listener) {
	server, done := s.server, make(chan struct{})
	s.listener, s.done, s.running = listener, done, true
	go func() {
		err := server.Serve(listener)
		s.mu.Lock()
		if err != nil && !errors.Is(err, http.ErrServerClosed) && !((s.stopped || s.retiring) && errors.Is(err, net.ErrClosed)) {
			s.serveErr = err
		}
		s.running = false
		close(done)
		s.mu.Unlock()
	}()
}

// ResumeAccepting binds the address again after StopAccepting, on the same server: a Windows hostd handover that
// did not complete leaves this generation serving.
func (s *HTTPService) ResumeAccepting(ctx context.Context) error {
	s.mu.Lock()
	done, retiring, stopped := s.done, s.retiring, s.stopped
	s.mu.Unlock()
	if !retiring || stopped || done == nil {
		return ErrHTTPState
	}
	select {
	case <-done:
	case <-ctx.Done():
		return ctx.Err()
	}
	listener, err := s.listen(ctx)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped || !s.retiring {
		return errors.Join(ErrHTTPState, listener.Close())
	}
	s.retiring = false
	s.serveLocked(listener)
	return nil
}

type protocolListener struct {
	net.Listener
	native  func(net.Conn)
	timeout time.Duration
}

func (l *protocolListener) Accept() (net.Conn, error) {
	for {
		conn, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		_ = conn.SetReadDeadline(time.Now().Add(l.timeout))
		var prefix [4]byte
		_, err = io.ReadFull(conn, prefix[:])
		_ = conn.SetReadDeadline(time.Time{})
		if err != nil {
			_ = conn.Close()
			continue
		}
		wrapped := &prefixedConn{Conn: conn, reader: io.MultiReader(bytes.NewReader(prefix[:]), conn)}
		if string(prefix[:]) == "PBT1" {
			go l.native(wrapped)
			continue
		}
		return wrapped, nil
	}
}

type prefixedConn struct {
	net.Conn
	reader io.Reader
}

func (c *prefixedConn) Read(p []byte) (int, error) { return c.reader.Read(p) }

// StopAccepting closes this process's copy of the listener and leaves accepted connections running; Shutdown
// later drains them. A retiring generation calls it first, so new connections on the socket it shares with
// the next generation reach that one at once.
func (s *HTTPService) StopAccepting() {
	s.mu.Lock()
	listener, running := s.listener, s.running
	s.retiring = true
	s.mu.Unlock()
	if running {
		_ = listener.Close()
	}
}

func (s *HTTPService) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	if s.stopped {
		err := s.serveErr
		s.mu.Unlock()
		return err
	}
	s.stopped = true
	server, listener, done, drain := s.server, s.listener, s.done, s.running || s.retiring
	s.mu.Unlock()
	if server == nil {
		return nil
	}
	var shutdownErr error
	if drain {
		// http.Server.Shutdown drops a connection whose request is read after it starts (net/http
		// conn.serve returns when shuttingDown after readRequest) and misses one Serve accepted but has
		// not tracked yet, such as one still sending the protocol prefix. A retiring generation must
		// answer every connection it accepted, as Envoy's hot-restart drain does: stop accepting, let
		// Serve return, then close idle connections and keep-alives until every accepted one is done.
		if err := listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			shutdownErr = err
		}
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for serving := done; serving != nil || s.connections.Load() > 0; {
			select {
			case <-serving:
				serving = nil
			case <-ticker.C:
			case <-ctx.Done():
				_ = server.Close()
				return errors.Join(shutdownErr, ctx.Err())
			}
			if serving == nil {
				server.SetKeepAlivesEnabled(false)
			}
		}
		shutdownErr = errors.Join(shutdownErr, server.Shutdown(ctx))
	}
	if done != nil {
		select {
		case <-done:
		case <-ctx.Done():
			_ = server.Close()
			return errors.Join(shutdownErr, ctx.Err())
		}
	}
	s.mu.Lock()
	serveErr := s.serveErr
	s.mu.Unlock()
	return errors.Join(shutdownErr, serveErr)
}

func (s *HTTPService) trackConnection(_ net.Conn, state http.ConnState) {
	switch state {
	case http.StateNew:
		s.connections.Add(1)
	case http.StateClosed, http.StateHijacked:
		s.connections.Add(-1)
	}
}

func (s *HTTPService) Address() net.Addr {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listener == nil {
		return nil
	}
	return s.listener.Addr()
}

func (s *HTTPService) Err() error { s.mu.Lock(); defer s.mu.Unlock(); return s.serveErr }

func LoopbackAddress(address string) bool {
	host, port, err := net.SplitHostPort(address)
	if err != nil || port == "" {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
