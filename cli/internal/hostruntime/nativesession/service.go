// Package nativesession connects direct Tailcat sessions to the existing host
// application handlers without entering the legacy carrier stack.
package nativesession

import (
	"context"
	"errors"
	"io"
	"net"
	"reflect"
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
	// ObserveFailure receives the original error once for each background
	// stream or transfer handler failure. Calls may be concurrent; Service
	// neither formats nor stores the error.
	ObserveFailure func(context.Context, FailureKind, error)
}

// FailureKind is the finite set of background application handlers whose
// errors Service consumes so one stream cannot stop the peer listener.
type FailureKind uint8

const (
	FailureStream FailureKind = iota + 1
	FailureTransfer
)

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
		if errors.Is(err, native.ErrStreamRejected) {
			continue
		}
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
	var (
		kind FailureKind
		err  error
	)
	if header.Consumer == "file_transfer" {
		kind = FailureTransfer
		err = s.config.ServeTransfer(ctx, connection)
	} else {
		kind = FailureStream
		err = s.config.ServeStream(ctx, header, connection)
	}
	if s.config.ObserveFailure != nil && !normalHandlerTermination(err) {
		s.config.ObserveFailure(ctx, kind, err)
	}
}

func normalHandlerTermination(err error) bool {
	if err == nil {
		return true
	}
	queue := []error{err}
	seen := make(map[error]struct{})
	visited := 0
	for len(queue) != 0 {
		if visited >= 16 {
			return false
		}
		current := queue[0]
		queue = queue[1:]
		visited++
		if current == nil {
			continue
		}
		typeOf := reflect.TypeOf(current)
		value := reflect.ValueOf(current)
		switch value.Kind() {
		case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
			if value.IsNil() {
				return false
			}
		}
		if typeOf.Comparable() {
			if _, exists := seen[current]; exists {
				return false
			}
			seen[current] = struct{}{}
		}
		switch wrapped := current.(type) {
		case interface{ Unwrap() []error }:
			causes := wrapped.Unwrap()
			if len(causes) == 0 || len(causes) > 16-visited-len(queue) {
				return false
			}
			added := false
			for _, cause := range causes {
				if cause != nil {
					queue = append(queue, cause)
					added = true
				}
			}
			if !added {
				return false
			}
		case interface{ Unwrap() error }:
			if cause := wrapped.Unwrap(); cause != nil {
				if visited+len(queue)+1 > 16 {
					return false
				}
				queue = append(queue, cause)
				continue
			}
			if !normalHandlerLeaf(current, typeOf) {
				return false
			}
		default:
			if !normalHandlerLeaf(current, typeOf) {
				return false
			}
		}
	}
	return true
}

func normalHandlerLeaf(err error, typeOf reflect.Type) bool {
	if !typeOf.Comparable() {
		return false
	}
	return err == io.EOF || err == net.ErrClosed || err == context.Canceled
}
