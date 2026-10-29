package server

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"sync"
	"time"
)

type releaseHTTPMutationKey struct{}

func ServeHTTPConnection(ctx context.Context, connection net.Conn, handler http.Handler) error {
	if ctx == nil || connection == nil || handler == nil {
		return ErrInvalidConfiguration
	}
	cached := &httpAdmission{ctx: ctx, connection: connection}
	defer cached.Close()
	ctx = context.WithValue(ctx, httpAdmissionKey{}, cached)
	server := &http.Server{}
	if admit, ok := ctx.Value(nativeHTTPAdmissionKey{}).(nativeHTTPAdmission); ok {
		var mu sync.Mutex
		var finish func()
		end := func() {
			mu.Lock()
			owned := finish
			finish = nil
			mu.Unlock()
			if owned != nil {
				owned()
			}
		}
		defer end()
		server.ConnState = func(_ net.Conn, state http.ConnState) {
			if state == http.StateIdle || state == http.StateClosed {
				// Idle follows finishRequest, including its final response flush.
				end()
			}
		}
		server.ReadHeaderTimeout = 10 * time.Second
		server.MaxHeaderBytes = 16 << 10
		server.IdleTimeout = 30 * time.Second
		actual := handler
		handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// net/http cancels r.Context before it flushes the final response.
			// The native admission instead ends at Idle/Closed below; the
			// resource handler still receives the cancelable request context.
			admissionCtx, cancelAdmission := context.WithCancel(ctx)
			release, lost, err := admit(admissionCtx)
			if err != nil || release == nil || lost == nil {
				cancelAdmission()
				if release != nil {
					release()
				}
				// Refusal precedes the resource handler and any request body read.
				// Close this stale association so the next request opens a fresh
				// authenticated stream; no mutation is replayed here.
				w.Header().Set("Connection", "close")
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusServiceUnavailable)
				_ = json.NewEncoder(w).Encode(struct {
					Code      string `json:"code"`
					Retryable bool   `json:"retryable"`
				}{"runtime_fenced", true})
				return
			}
			requestCtx, cancel := context.WithCancel(r.Context())
			stop := make(chan struct{})
			done := make(chan struct{})
			go func() {
				defer close(done)
				select {
				case <-lost:
					cancel()
					_ = connection.Close()
				case <-stop:
				}
			}()
			var once sync.Once
			mu.Lock()
			releaseMutation := func() { once.Do(func() { close(stop); <-done; release(); cancelAdmission() }) }
			finish = func() { releaseMutation(); cancel() }
			mu.Unlock()
			actual.ServeHTTP(w, r.WithContext(context.WithValue(requestCtx, releaseHTTPMutationKey{}, releaseMutation)))
		})
	}
	return serveHTTPConnection(ctx, connection, handler, server)
}

// ServeInspectorHTTPConnection serves exactly one bounded inspector exchange on
// an already authenticated stream. It never opens a listener or a target socket.
func ServeInspectorHTTPConnection(ctx context.Context, connection net.Conn, handler http.Handler) error {
	server := &http.Server{MaxHeaderBytes: 16 << 10, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 45 * time.Second, WriteTimeout: 45 * time.Second}
	server.SetKeepAlivesEnabled(false)
	return serveHTTPConnection(ctx, connection, handler, server)
}
func serveHTTPConnection(ctx context.Context, connection net.Conn, handler http.Handler, server *http.Server) error {
	if ctx == nil || connection == nil || handler == nil {
		return ErrInvalidConfiguration
	}
	tracked := &trackedHTTPConn{Conn: connection, closed: make(chan struct{})}
	listener := &singleHTTPListener{ctx: ctx, connection: tracked}
	server.Handler = handler
	server.BaseContext = func(net.Listener) context.Context { return ctx }
	stop := context.AfterFunc(ctx, func() { _ = tracked.Close() })
	err := server.Serve(listener)
	stop()
	if errors.Is(err, http.ErrServerClosed) || errors.Is(err, net.ErrClosed) || ctx.Err() != nil {
		return nil
	}
	return err
}

type trackedHTTPConn struct {
	net.Conn
	closed chan struct{}
	once   sync.Once
}

func (c *trackedHTTPConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() { close(c.closed) })
	return err
}

type singleHTTPListener struct {
	ctx        context.Context
	connection *trackedHTTPConn
	once       sync.Once
	accepted   bool
}

func (l *singleHTTPListener) Accept() (net.Conn, error) {
	if !l.accepted {
		l.accepted = true
		return l.connection, nil
	}
	select {
	case <-l.connection.closed:
		return nil, net.ErrClosed
	case <-l.ctx.Done():
		return nil, net.ErrClosed
	}
}

func (l *singleHTTPListener) Close() error {
	l.once.Do(func() { _ = l.connection.Close() })
	return nil
}

func (l *singleHTTPListener) Addr() net.Addr { return l.connection.LocalAddr() }
