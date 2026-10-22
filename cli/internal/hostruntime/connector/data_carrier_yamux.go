package connector

import (
	"context"
	"io"
	"net"
	"time"

	yamux "github.com/libp2p/go-yamux/v5"
	sharedprotocol "github.com/pinksaucepasta/paperboat/shared/connectorprotocol"
)

type yamuxDataCarrierSession struct {
	session *yamux.Session
}

func newYamuxDataCarrierSession(link io.ReadWriteCloser, config DataCarrierConfig, client bool) (DataCarrierSession, error) {
	yamuxConfig := yamux.DefaultConfig()
	yamuxConfig.LogOutput = io.Discard
	yamuxConfig.AcceptBacklog = config.AcceptBacklog
	yamuxConfig.EnableKeepAlive = true
	yamuxConfig.KeepAliveInterval = config.KeepAliveInterval
	yamuxConfig.ConnectionWriteTimeout = config.ConnectionWriteLimit
	yamuxConfig.MaxStreamWindowSize = max(config.StreamWindow, sharedprotocol.DataCarrierMaxStreamWindow)
	yamuxConfig.InitialStreamWindowSize = config.StreamWindow
	yamuxConfig.MaxIncomingStreams = uint32(config.MaximumStreams)
	connection := asDataCarrierNetConn(link)
	var (
		session *yamux.Session
		err     error
	)
	if client {
		session, err = yamux.Client(connection, yamuxConfig, nil)
	} else {
		session, err = yamux.Server(connection, yamuxConfig, nil)
	}
	if err != nil {
		return nil, err
	}
	return &yamuxDataCarrierSession{session: session}, nil
}

func (s *yamuxDataCarrierSession) OpenStream(ctx context.Context) (DataCarrierStreamLink, error) {
	if s == nil || s.session == nil || ctx == nil {
		return nil, ErrInvalidDataCarrierConfig
	}
	return s.session.OpenStream(ctx)
}

type dataCarrierNetConn struct{ io.ReadWriteCloser }

func asDataCarrierNetConn(link io.ReadWriteCloser) net.Conn {
	if connection, ok := link.(net.Conn); ok {
		return connection
	}
	return &dataCarrierNetConn{ReadWriteCloser: link}
}

func (c *dataCarrierNetConn) LocalAddr() net.Addr { return dataCarrierAddr("local") }

func (c *dataCarrierNetConn) RemoteAddr() net.Addr { return dataCarrierAddr("remote") }

func (c *dataCarrierNetConn) SetDeadline(t time.Time) error {
	if v, ok := c.ReadWriteCloser.(interface{ SetDeadline(time.Time) error }); ok {
		return v.SetDeadline(t)
	}
	return nil
}

func (c *dataCarrierNetConn) SetReadDeadline(t time.Time) error {
	if v, ok := c.ReadWriteCloser.(interface{ SetReadDeadline(time.Time) error }); ok {
		return v.SetReadDeadline(t)
	}
	return nil
}

func (c *dataCarrierNetConn) SetWriteDeadline(t time.Time) error {
	if v, ok := c.ReadWriteCloser.(interface{ SetWriteDeadline(time.Time) error }); ok {
		return v.SetWriteDeadline(t)
	}
	return nil
}

type dataCarrierAddr string

func (a dataCarrierAddr) Network() string { return "carrier" }

func (a dataCarrierAddr) String() string { return string(a) }

func (s *yamuxDataCarrierSession) AcceptStream(ctx context.Context) (DataCarrierStreamLink, error) {
	if s == nil || s.session == nil || ctx == nil {
		return nil, ErrInvalidDataCarrierConfig
	}
	result := make(chan struct {
		stream *yamux.Stream
		err    error
	}, 1)
	go func() {
		stream, err := s.session.AcceptStream()
		result <- struct {
			stream *yamux.Stream
			err    error
		}{stream: stream, err: err}
	}()
	select {
	case opened := <-result:
		return opened.stream, opened.err
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.session.CloseChan():
		return nil, ErrDataCarrierClosed
	}
}

func (s *yamuxDataCarrierSession) Ping(ctx context.Context) error {
	if s == nil || s.session == nil || ctx == nil {
		return ErrInvalidDataCarrierConfig
	}
	result := make(chan error, 1)
	go func() {
		_, err := s.session.Ping()
		result <- err
	}()
	select {
	case err := <-result:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-s.session.CloseChan():
		return ErrDataCarrierClosed
	}
}

func (s *yamuxDataCarrierSession) Close() error {
	if s == nil || s.session == nil {
		return nil
	}
	return s.session.Close()
}

func (s *yamuxDataCarrierSession) CloseChan() <-chan struct{} {
	if s == nil || s.session == nil {
		return closedDataCarrierChannel()
	}
	return s.session.CloseChan()
}
