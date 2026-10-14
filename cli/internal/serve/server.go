package serve

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"time"
)

const DefaultDrainTimeout = 10 * time.Second

type Server struct {
	listener net.Listener
	http     *http.Server
	done     chan error
	closer   io.Closer
	close    sync.Once
	closeErr error
}

func Start(handler http.Handler) (*Server, error) {
	return StartLoopback(handler, 0)
}

func StartLoopback(handler http.Handler, port uint16) (*Server, error) {
	if handler == nil {
		return nil, ErrInvalidSource
	}
	listener, err := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", fmt.Sprint(port)))
	if err != nil {
		var closeErr error
		if closer, ok := handler.(io.Closer); ok {
			closeErr = serveOperationFailure("could not release local serve handler", closer.Close())
		}
		return nil, errors.Join(serveOperationFailure("could not start local serve listener", err), closeErr)
	}
	server := &Server{listener: listener, http: &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second}, done: make(chan error, 1)}
	server.closer, _ = handler.(io.Closer)
	go func() {
		err := server.http.Serve(listener)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		} else if err != nil {
			err = serveOperationFailure("local serve listener stopped unexpectedly", err)
		}
		server.done <- err
		close(server.done)
	}()
	return server, nil
}

func (s *Server) Port() uint16 {
	return uint16(s.listener.Addr().(*net.TCPAddr).Port)
}

func (s *Server) Done() <-chan error { return s.done }

func (s *Server) Shutdown(ctx context.Context) error {
	if ctx == nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.Background(), DefaultDrainTimeout)
		defer cancel()
	}
	shutdownErr := s.http.Shutdown(ctx)
	listenerErr := s.listener.Close()
	if errors.Is(listenerErr, net.ErrClosed) {
		listenerErr = nil
	}
	listenerErr = serveOperationFailure("could not close local serve listener", listenerErr)
	s.close.Do(func() {
		if s.closer != nil {
			s.closeErr = serveOperationFailure("could not close local serve handler", s.closer.Close())
		}
	})
	return errors.Join(shutdownErr, listenerErr, s.closeErr)
}
