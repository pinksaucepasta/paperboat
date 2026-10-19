package connectorrotation

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"strconv"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/connectorprotocol"
)

// EnqueueFrame schedules one host-to-server control frame for the Serve loop.
// The queue is bounded and the caller retains ownership of the frame. No
// caller may write directly to the carrier while Serve is running.
func (s *ControlSession) EnqueueFrame(frame connectorprotocol.Frame) error {
	if s == nil || s.outbound == nil {
		return ErrControlSessionInvalid
	}
	if err := frame.Validate(); err != nil {
		return err
	}
	switch frame.Type {
	case connectorprotocol.MessageHeartbeat, connectorprotocol.MessageAuthRenew, connectorprotocol.MessageCredentialRotationReady, connectorprotocol.MessageDisconnect:
	default:
		return connectorprotocol.ErrUnsupportedMessage
	}
	select {
	case s.outbound <- frame:
		return nil
	default:
		return ErrControlSessionOutboundFull
	}
}

func (s *ControlSession) QueueHeartbeat(now time.Time) error {
	if s == nil {
		return ErrControlSessionInvalid
	}
	frame, err := s.HeartbeatFrame(s.nextRequestID("heartbeat"), now)
	if err != nil {
		return err
	}
	return s.EnqueueFrame(frame)
}

func (s *ControlSession) QueueRenewal(now time.Time, nonce, signedProof string) error {
	if s == nil {
		return ErrControlSessionInvalid
	}
	frame, err := s.RenewalFrame(s.nextRequestID("renewal"), now, nonce, signedProof)
	if err != nil {
		return err
	}
	return s.EnqueueFrame(frame)
}

func (s *ControlSession) nextRequestID(prefix string) string {
	s.mu.Lock()
	s.requestSeq++
	sequence := s.requestSeq
	s.mu.Unlock()
	if prefix == "" {
		prefix = "event"
	}
	return prefix + "-" + strconv.FormatUint(sequence, 10)
}

func (s *ControlSession) HelloFrame(requestID string) (connectorprotocol.Frame, error) {
	if s == nil || s.client == nil {
		return connectorprotocol.Frame{}, ErrControlSessionInvalid
	}
	return connectorprotocol.NewFrame(connectorprotocol.MessageHello, requestID, s.client.Hello())
}

func (s *ControlSession) HeartbeatFrame(requestID string, now time.Time) (connectorprotocol.Frame, error) {
	if s == nil || s.client == nil {
		return connectorprotocol.Frame{}, ErrControlSessionInvalid
	}
	heartbeat, err := s.client.Heartbeat(now)
	if err != nil {
		return connectorprotocol.Frame{}, err
	}
	return connectorprotocol.NewFrame(connectorprotocol.MessageHeartbeat, requestID, heartbeat)
}

func (s *ControlSession) RenewalFrame(requestID string, now time.Time, nonce, signedProof string) (connectorprotocol.Frame, error) {
	if s == nil || s.client == nil {
		return connectorprotocol.Frame{}, ErrControlSessionInvalid
	}
	request, err := s.client.RenewalRequest(now, nonce, signedProof)
	if err != nil {
		return connectorprotocol.Frame{}, err
	}
	return connectorprotocol.NewFrame(connectorprotocol.MessageAuthRenew, requestID, request)
}

func (s *ControlSession) signedRenewalFrame(ctx context.Context, requestID string, now time.Time) (connectorprotocol.Frame, error) {
	if s == nil || s.client == nil || s.renewalSigner == nil || ctx == nil {
		return connectorprotocol.Frame{}, ErrControlSessionInvalid
	}
	s.mu.RLock()
	welcome, welcomed := s.welcome, s.welcomed
	s.mu.RUnlock()
	if !welcomed {
		return connectorprotocol.Frame{}, connectorprotocol.ErrSessionConflict
	}
	var randomNonce [24]byte
	if _, err := rand.Read(randomNonce[:]); err != nil {
		return connectorprotocol.Frame{}, err
	}
	auth := s.client.Auth()
	request := connectorprotocol.RenewalRequest{
		SessionID: welcome.SessionID, AccountID: auth.AccountID, TunnelID: auth.TunnelID,
		ConnectorID: auth.ConnectorID, HostID: auth.HostID, IdentityKeyID: auth.IdentityKeyID,
		IdentityKeyThumbprint: auth.IdentityKeyThumbprint, ProcessGeneration: s.client.Hello().ProcessGeneration,
		CredentialGeneration: auth.CredentialGeneration, Nonce: base64.RawURLEncoding.EncodeToString(randomNonce[:]), RequestedAt: now,
	}
	payload, err := connectorprotocol.RenewalProofPayload(request)
	if err != nil {
		return connectorprotocol.Frame{}, err
	}
	signature, err := s.renewalSigner.SignRenewalProof(ctx, payload)
	if err != nil || len(signature) == 0 {
		return connectorprotocol.Frame{}, errors.Join(ErrControlSessionInvalid, err)
	}
	request.SignedProof = base64.RawURLEncoding.EncodeToString(signature)
	if err := request.ValidateAt(now); err != nil {
		return connectorprotocol.Frame{}, err
	}
	return connectorprotocol.NewFrame(connectorprotocol.MessageAuthRenew, requestID, request)
}

// ReplacementReadyFrame is the explicit asynchronous readiness boundary for
// a credential install. The caller invokes it from a supervisor/event worker,
// never from HandleFrame's receive loop. It may wait for process and
// data-plane readiness through ReadinessSource, then validates that observation
// against the replacement ClientSession and the active host configuration
// before emitting the credential-aware ready frame.
func (s *ControlSession) ReplacementReadyFrame(ctx context.Context, requestID string) (connectorprotocol.Frame, error) {
	if s == nil || s.client == nil || s.rotation == nil || s.readiness == nil || ctx == nil {
		return connectorprotocol.Frame{}, ErrControlSessionInvalid
	}
	record, ok := s.rotation.Record()
	if !ok || record.Install == nil {
		return connectorprotocol.Frame{}, ErrOperationNotFound
	}
	if err := s.requireRotationRuntime(); err != nil {
		return connectorprotocol.Frame{}, err
	}
	observation, err := s.readiness.WaitReplacementReady(ctx, *record.Install)
	if err != nil {
		return s.rotationFailureFrame(requestID, *record.Install, err)
	}
	return s.MarkRotationReadyFrame(ctx, requestID, observation)
}

func (s *ControlSession) CloseFrame(requestID string, reason connectorprotocol.DisconnectReason) (connectorprotocol.Frame, error) {
	if s == nil || s.client == nil {
		return connectorprotocol.Frame{}, ErrControlSessionInvalid
	}
	if err := s.client.Close(reason); err != nil {
		return connectorprotocol.Frame{}, err
	}
	disconnect := s.client.Disconnect()
	return connectorprotocol.NewFrame(connectorprotocol.MessageDisconnect, requestID, disconnect)
}
