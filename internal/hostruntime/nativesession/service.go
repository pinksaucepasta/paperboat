// Package nativesession connects direct Tailcat sessions to the existing host
// application handlers without entering the legacy carrier stack.
package nativesession

import (
	"context"
	"errors"
	"net"
	"sync"

	"github.com/pinksaucepasta/paperboat/internal/peertransport/native"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/streamauth"
)

var ErrInvalid = errors.New("invalid native application session")

type Config struct {
	Authorize     func(context.Context, streamauth.Header) (string, error)
	ServeStream   func(context.Context, streamauth.Header, net.Conn) error
	ServeTransfer func(context.Context, net.Conn) error
	ServeHTTP3    func(context.Context, *native.Session, func(context.Context, streamauth.Header) (string, error)) error
}

type Service struct {
	config  Config
	workers sync.WaitGroup
}

func New(config Config) (*Service, error) {
	if config.Authorize == nil || config.ServeStream == nil || config.ServeTransfer == nil {
		return nil, ErrInvalid
	}
	return &Service{config: config}, nil
}

func (s *Service) Serve(ctx context.Context, session *native.Session) error {
	if ctx == nil || session == nil {
		return ErrInvalid
	}
	if session.IsPrivateHTTP3() {
		if s.config.ServeHTTP3 == nil {
			return ErrInvalid
		}
		return s.config.ServeHTTP3(ctx, session, s.config.Authorize)
	}
	for {
		var authorizationResource string
		connection, header, err := session.AcceptAuthorized(ctx, func(authorizeCtx context.Context, value streamauth.Header) (string, error) {
			var authorizeErr error
			authorizationResource, authorizeErr = s.config.Authorize(authorizeCtx, value)
			return authorizationResource, authorizeErr
		})
		if err != nil {
			return err
		}
		s.workers.Add(1)
		go func() {
			defer s.workers.Done()
			s.serveOne(ctx, header, connection)
		}()
	}
}

// Wait joins application handlers after the owning native sessions are closed.
func (s *Service) Wait() { s.workers.Wait() }

func (s *Service) serveOne(ctx context.Context, header streamauth.Header, connection net.Conn) {
	defer connection.Close()
	if header.Consumer == "file_transfer" {
		_ = s.config.ServeTransfer(ctx, connection)
		return
	}
	_ = s.config.ServeStream(ctx, header, connection)
}
