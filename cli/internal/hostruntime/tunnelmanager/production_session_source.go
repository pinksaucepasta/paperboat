package tunnelmanager

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"sync"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/connectorprotocol"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/connector"
)

type liveDataCarrierSessionSource struct {
	source            connector.DataCarrierSessionSource
	expectedAccountID string
}

type welcomeDataCarrierSessionSource struct {
	factory     CarrierDescriptorSource
	reconnect   func(string)
	hello       connectorprotocol.Hello
	mu          sync.RWMutex
	welcome     connectorprotocol.Welcome
	ready       chan struct{}
	readyClosed bool
}

func (s *welcomeDataCarrierSessionSource) ObserveWelcome(welcome connectorprotocol.Welcome) error {
	if s == nil || s.factory == nil || welcome.SessionID == "" {
		return ErrProductionIdentityMissing
	}
	s.mu.Lock()
	s.welcome = welcome
	if s.ready == nil {
		s.ready = make(chan struct{})
	}
	ready := s.ready
	if !s.readyClosed {
		close(ready)
		s.readyClosed = true
	}
	s.mu.Unlock()
	return nil
}

// UpdateHello advances the control identity expected by the deferred carrier
// source. A new control session must carry the same durable connector binding
// and a strictly higher process generation. Resetting the readiness gate makes
// a concurrent reconciliation wait for the matching reconnect Welcome instead
// of pairing a fresh Hello with the prior session.
func (s *welcomeDataCarrierSessionSource) UpdateHello(hello connectorprotocol.Hello) error {
	if s == nil || s.factory == nil {
		return errors.Join(ErrProductionIdentityMissing, ErrProductionAssemblyInvalid)
	}
	if err := hello.Validate(time.Time{}); err != nil {
		return errors.Join(ErrProductionIdentityMissing, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.hello.AccountID != hello.AccountID || s.hello.TunnelID != hello.TunnelID || s.hello.ConnectorID != hello.ConnectorID || s.hello.HostID != hello.HostID || hello.ProcessGeneration <= s.hello.ProcessGeneration {
		return errors.Join(ErrGenerationConflict, ErrProductionIdentityMissing)
	}
	s.hello = hello
	s.ready = make(chan struct{})
	s.readyClosed = false
	return nil
}

func (s *welcomeDataCarrierSessionSource) PrepareDataCarrier(ctx context.Context, request ApplyRequest) (connector.DataCarrierPrepareRequest, error) {
	if s == nil || ctx == nil || s.factory == nil {
		return connector.DataCarrierPrepareRequest{}, ErrProductionAssemblyInvalid
	}
	s.mu.Lock()
	if s.ready == nil {
		s.ready = make(chan struct{})
	}
	ready := s.ready
	s.mu.Unlock()
	select {
	case <-ready:
	case <-ctx.Done():
		return connector.DataCarrierPrepareRequest{}, ctx.Err()
	}
	s.mu.RLock()
	welcome := s.welcome
	hello := s.hello
	s.mu.RUnlock()
	source, err := s.factory(ctx, hello, welcome, request)
	if errors.Is(err, ErrSessionStale) && s.reconnect != nil {
		s.reconnect(welcome.SessionID)
	}
	if err != nil {
		return connector.DataCarrierPrepareRequest{}, err
	}
	prepared, err := source.PrepareDataCarrier(ctx)
	if err != nil {
		return connector.DataCarrierPrepareRequest{}, err
	}
	identity := prepared.Identity
	if identity.AccountID != hello.AccountID || identity.TunnelID != hello.TunnelID || identity.ConnectorID != hello.ConnectorID || identity.HostID != hello.HostID || identity.SessionID != welcome.SessionID || identity.ProcessGeneration != hello.ProcessGeneration || identity.Generation != request.Snapshot.Generation {
		return connector.DataCarrierPrepareRequest{}, errors.Join(ErrGenerationConflict, ErrProductionIdentityMissing)
	}
	return prepared, nil
}

// welcomeObservingControlStream keeps the bootstrap transport opaque to the
// control state machine while exposing the authenticated Welcome identity to
// the carrier session source. It observes complete connector-v1 frames only;
// all validation and dispatch remain owned by ControlSession.Serve.
type welcomeObservingControlStream struct {
	io.ReadWriteCloser
	onWelcome func(connectorprotocol.Welcome) error

	mu       sync.Mutex
	pending  []byte
	observed bool
	err      error
}

func (s *welcomeObservingControlStream) Read(p []byte) (int, error) {
	if s == nil || s.ReadWriteCloser == nil {
		return 0, ErrProductionControlMissing
	}
	s.mu.Lock()
	if s.err != nil {
		err := s.err
		s.mu.Unlock()
		return 0, err
	}
	s.mu.Unlock()
	n, err := s.ReadWriteCloser.Read(p)
	if n <= 0 {
		return n, err
	}
	s.mu.Lock()
	s.pending = append(s.pending, p[:n]...)
	callbackErr := s.observeLocked()
	if callbackErr != nil {
		s.err = callbackErr
	}
	s.mu.Unlock()
	// Return bytes already read so ControlSession can process a valid Welcome;
	// surface an observer failure on the next read without losing the frame.
	if callbackErr != nil && err == nil && n == 0 {
		err = callbackErr
	}
	return n, err
}

func (s *welcomeObservingControlStream) observeLocked() error {
	if s.observed || s.onWelcome == nil {
		return nil
	}
	for {
		if len(s.pending) < 4 {
			return nil
		}
		length := int(binary.BigEndian.Uint32(s.pending[:4]))
		if length <= 0 || length > connectorprotocol.MaxFrameBytes {
			// Leave malformed-frame handling to ReadFrame and stop buffering so a
			// peer cannot grow this observer beyond one bounded frame.
			s.pending = nil
			return nil
		}
		total := 4 + length
		if len(s.pending) < total {
			return nil
		}
		frame, err := connectorprotocol.ReadFrame(bytes.NewReader(s.pending[:total]))
		if err != nil {
			return nil
		}
		s.pending = append([]byte(nil), s.pending[total:]...)
		if frame.Type != connectorprotocol.MessageWelcome {
			continue
		}
		var welcome connectorprotocol.Welcome
		if err := frame.DecodePayload(&welcome); err != nil {
			return err
		}
		if err := s.onWelcome(welcome); err != nil {
			return err
		}
		s.observed = true
		return nil
	}
}

func (s *welcomeObservingControlStream) SetDeadline(deadline time.Time) error {
	if setter, ok := s.ReadWriteCloser.(interface{ SetDeadline(time.Time) error }); ok {
		return setter.SetDeadline(deadline)
	}
	return nil
}

func (s *welcomeObservingControlStream) SetReadDeadline(deadline time.Time) error {
	if setter, ok := s.ReadWriteCloser.(interface{ SetReadDeadline(time.Time) error }); ok {
		return setter.SetReadDeadline(deadline)
	}
	return nil
}

func (s *welcomeObservingControlStream) SetWriteDeadline(deadline time.Time) error {
	if setter, ok := s.ReadWriteCloser.(interface{ SetWriteDeadline(time.Time) error }); ok {
		return setter.SetWriteDeadline(deadline)
	}
	return nil
}

func (s liveDataCarrierSessionSource) PrepareDataCarrier(ctx context.Context, request ApplyRequest) (connector.DataCarrierPrepareRequest, error) {
	if ctx == nil {
		return connector.DataCarrierPrepareRequest{}, ErrProductionAssemblyInvalid
	}
	prepared, err := s.source.PrepareDataCarrier(ctx)
	if err != nil {
		return connector.DataCarrierPrepareRequest{}, err
	}
	identity := prepared.Identity
	if identity.AccountID != s.expectedAccountID || identity.TunnelID != request.Tunnel.ID || identity.ConnectorID != request.Connector.ID || identity.HostID != request.Connector.HostID || identity.Generation != request.Snapshot.Generation {
		return connector.DataCarrierPrepareRequest{}, errors.Join(ErrGenerationConflict, ErrProductionIdentityMissing)
	}
	return prepared, nil
}
