package connectorrotation

import (
	"context"
	"errors"

	"github.com/pinksaucepasta/paperboat/internal/connectorprotocol"
)

func (s *ControlSession) validateBoundRotation(identityAccount, tunnelID, connectorID, hostID, sessionID string, processGeneration uint64) error {
	s.mu.RLock()
	welcomed, welcome := s.welcomed, s.welcome
	s.mu.RUnlock()
	if !welcomed {
		return connectorprotocol.ErrSessionConflict
	}
	if err := s.requireRotationRuntime(); err != nil {
		return err
	}
	if err := s.client.CheckLease(s.clock.Now().UTC()); err != nil {
		return err
	}
	hello := s.client.Hello()
	if identityAccount != hello.AccountID || tunnelID != hello.TunnelID || connectorID != hello.ConnectorID || hostID != hello.HostID || sessionID != welcome.SessionID || processGeneration != hello.ProcessGeneration {
		return connectorprotocol.ErrIdentityMismatch
	}
	if s.client.State() != connectorprotocol.SessionReady {
		return &connectorprotocol.Error{
			Code:      connectorprotocol.CodeCredentialRotationNotReady,
			Reason:    connectorprotocol.ReasonSnapshotRejected,
			Retryable: true,
			Cause:     errors.New("active connector session is not ready for credential rotation"),
		}
	}
	return nil
}

func hasControlCapability(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func rotationCapabilityError() error {
	return &connectorprotocol.Error{
		Code:      connectorprotocol.CodeCapabilityMissing,
		Reason:    connectorprotocol.ReasonCapabilityMissing,
		Retryable: false,
		Cause:     errors.New("credential rotation capability is not negotiated"),
	}
}

func (s *ControlSession) requireRotationRuntime() error {
	if s == nil || s.client == nil || s.rotation == nil {
		return &connectorprotocol.Error{
			Code:      connectorprotocol.CodeCredentialRotationFailed,
			Reason:    connectorprotocol.ReasonCredentialRotation,
			Retryable: true,
			Cause:     errors.New("credential rotation runtime is unavailable"),
		}
	}
	s.mu.RLock()
	welcomed, welcome := s.welcomed, s.welcome
	s.mu.RUnlock()
	if !welcomed || !hasControlCapability(welcome.Capabilities, connectorprotocol.CapabilityCredentialRotation) || !hasControlCapability(s.client.Hello().Capabilities, connectorprotocol.CapabilityCredentialRotation) {
		return rotationCapabilityError()
	}
	return nil
}

func identitySessionID(session *connectorprotocol.ClientSession) string {
	if session == nil {
		return ""
	}
	return session.Disconnect().SessionID
}

func validateReplacementReadiness(observation ReplacementReadiness, install connectorprotocol.CredentialRotationInstall, oldSession *connectorprotocol.ClientSession, clock connectorprotocol.Clock) error {
	if observation.Session == nil || oldSession == nil {
		return &connectorprotocol.Error{Code: connectorprotocol.CodeCredentialRotationNotReady, Reason: connectorprotocol.ReasonSnapshotRejected, Retryable: true, Cause: errors.New("replacement session is unavailable")}
	}
	if !hasControlCapability(observation.NegotiatedCapabilities, connectorprotocol.CapabilityCredentialRotation) || !hasControlCapability(observation.Session.Hello().Capabilities, connectorprotocol.CapabilityCredentialRotation) {
		return rotationCapabilityError()
	}
	if !observation.EdgeReady || !observation.RouteReady || !observation.OriginReady {
		return &connectorprotocol.Error{Code: connectorprotocol.CodeCredentialRotationNotReady, Reason: connectorprotocol.ReasonSnapshotRejected, Retryable: true, Cause: errors.New("replacement readiness is incomplete")}
	}
	if connectorprotocol.ValidateIdentifier(observation.SessionID) != nil || observation.ProcessGeneration == 0 || observation.ConfigGeneration == 0 {
		return connectorprotocol.ErrInvalidInput
	}
	readiness := connectorprotocol.Readiness{
		AccountID: oldSession.Hello().AccountID, TunnelID: oldSession.Hello().TunnelID, ConnectorID: oldSession.Hello().ConnectorID,
		SessionID: observation.SessionID, ProcessGeneration: observation.ProcessGeneration,
		Generation: observation.ConfigGeneration, ContentHash: observation.ConfigContentHash,
		EdgeReady: observation.EdgeReady, RouteReady: observation.RouteReady, OriginReady: observation.OriginReady,
	}
	if err := readiness.Validate(); err != nil {
		return err
	}
	replacement := observation.Session
	if replacement.State() != connectorprotocol.SessionReady {
		return &connectorprotocol.Error{Code: connectorprotocol.CodeCredentialRotationNotReady, Reason: connectorprotocol.ReasonSnapshotRejected, Retryable: true, Cause: errors.New("replacement session is not ready")}
	}
	if clock != nil {
		if err := replacement.CheckLease(clock.Now().UTC()); err != nil {
			return err
		}
	}
	identity := replacement.Hello()
	replacementID := identitySessionID(replacement)
	if replacementID != observation.SessionID || identity.ProcessGeneration != observation.ProcessGeneration || replacementID == install.SessionID || observation.ProcessGeneration < install.ReplacementProcessGeneration {
		return connectorprotocol.ErrIdentityMismatch
	}
	old := oldSession.Hello()
	if identity.AccountID != old.AccountID || identity.TunnelID != old.TunnelID || identity.ConnectorID != old.ConnectorID || identity.HostID != old.HostID {
		return connectorprotocol.ErrIdentityMismatch
	}
	active, ok := replacement.Active()
	if !ok || active.Generation != observation.ConfigGeneration || active.ContentHash != observation.ConfigContentHash {
		return &connectorprotocol.Error{Code: connectorprotocol.CodeContentHashMismatch, Reason: connectorprotocol.ReasonSnapshotRejected, Retryable: true, Cause: errors.New("replacement active configuration does not match readiness")}
	}
	if active.AccountID != old.AccountID || active.TunnelID != old.TunnelID || active.ConnectorID != old.ConnectorID || active.SessionID != observation.SessionID || active.ProcessGeneration != observation.ProcessGeneration {
		return connectorprotocol.ErrIdentityMismatch
	}
	return nil
}

func (s *ControlSession) ensureRotationRecord(ctx context.Context, operationID string, allowNotFound bool) error {
	if s.rotation == nil {
		return connectorprotocol.ErrUnsupportedMessage
	}
	if record, ok := s.rotation.Record(); ok && record.OperationID == operationID {
		return nil
	}
	err := s.rotation.Recover(ctx, operationID)
	if allowNotFound && errorLeavesMatch(err, ErrOperationNotFound) {
		return nil
	}
	return err
}

func (s *ControlSession) handleRotationChallenge(ctx context.Context, frame connectorprotocol.Frame) ([]connectorprotocol.Frame, error) {
	var challenge connectorprotocol.CredentialRotationChallenge
	if err := frame.DecodePayload(&challenge); err != nil {
		return nil, err
	}
	if err := s.validateBoundRotation(challenge.AccountID, challenge.TunnelID, challenge.ConnectorID, challenge.HostID, challenge.SessionID, challenge.ProcessGeneration); err != nil {
		return s.rotationError(frame.RequestID, challenge.AccountID, challenge.TunnelID, challenge.OperationID, challenge.ConnectorID, challenge.HostID, challenge.SessionID, challenge.ProcessGeneration, challenge.TargetSetHash, challenge.OldCredentialGeneration, challenge.NewCredentialGeneration, err)
	}
	hello := s.client.Hello()
	if challenge.OldIdentityKeyID != hello.Auth.IdentityKeyID || challenge.OldIdentityKeyThumbprint != hello.Auth.IdentityKeyThumbprint {
		err := connectorprotocol.ErrIdentityMismatch
		return s.rotationError(frame.RequestID, challenge.AccountID, challenge.TunnelID, challenge.OperationID, challenge.ConnectorID, challenge.HostID, challenge.SessionID, challenge.ProcessGeneration, challenge.TargetSetHash, challenge.OldCredentialGeneration, challenge.NewCredentialGeneration, err)
	}
	if err := s.ensureRotationRecord(ctx, challenge.OperationID, true); err != nil {
		return s.rotationError(frame.RequestID, challenge.AccountID, challenge.TunnelID, challenge.OperationID, challenge.ConnectorID, challenge.HostID, challenge.SessionID, challenge.ProcessGeneration, challenge.TargetSetHash, challenge.OldCredentialGeneration, challenge.NewCredentialGeneration, err)
	}
	if s.rotation == nil {
		return s.rotationError(frame.RequestID, challenge.AccountID, challenge.TunnelID, challenge.OperationID, challenge.ConnectorID, challenge.HostID, challenge.SessionID, challenge.ProcessGeneration, challenge.TargetSetHash, challenge.OldCredentialGeneration, challenge.NewCredentialGeneration, connectorprotocol.ErrUnsupportedMessage)
	}
	proof, err := s.rotation.AcceptChallenge(ctx, challenge)
	if err != nil {
		return s.rotationError(frame.RequestID, challenge.AccountID, challenge.TunnelID, challenge.OperationID, challenge.ConnectorID, challenge.HostID, challenge.SessionID, challenge.ProcessGeneration, challenge.TargetSetHash, challenge.OldCredentialGeneration, challenge.NewCredentialGeneration, err)
	}
	return oneFrame(connectorprotocol.NewFrame(connectorprotocol.MessageCredentialRotationProof, frame.RequestID, proof))
}

func (s *ControlSession) handleRotationInstall(ctx context.Context, frame connectorprotocol.Frame) ([]connectorprotocol.Frame, error) {
	var install connectorprotocol.CredentialRotationInstall
	if err := frame.DecodePayload(&install); err != nil {
		return nil, err
	}
	if err := s.validateBoundRotation(install.AccountID, install.TunnelID, install.ConnectorID, install.HostID, install.SessionID, install.ProcessGeneration); err != nil {
		return s.rotationError(frame.RequestID, install.AccountID, install.TunnelID, install.OperationID, install.ConnectorID, install.HostID, install.SessionID, install.ProcessGeneration, install.TargetSetHash, install.OldCredentialGeneration, install.NewCredentialGeneration, err)
	}
	if err := s.ensureRotationRecord(ctx, install.OperationID, false); err != nil {
		return s.rotationError(frame.RequestID, install.AccountID, install.TunnelID, install.OperationID, install.ConnectorID, install.HostID, install.SessionID, install.ProcessGeneration, install.TargetSetHash, install.OldCredentialGeneration, install.NewCredentialGeneration, err)
	}
	if s.rotation == nil {
		return s.rotationError(frame.RequestID, install.AccountID, install.TunnelID, install.OperationID, install.ConnectorID, install.HostID, install.SessionID, install.ProcessGeneration, install.TargetSetHash, install.OldCredentialGeneration, install.NewCredentialGeneration, connectorprotocol.ErrUnsupportedMessage)
	}
	ack, err := s.rotation.AcceptInstall(ctx, install)
	if err != nil {
		return s.rotationError(frame.RequestID, install.AccountID, install.TunnelID, install.OperationID, install.ConnectorID, install.HostID, install.SessionID, install.ProcessGeneration, install.TargetSetHash, install.OldCredentialGeneration, install.NewCredentialGeneration, err)
	}
	response, frameErr := connectorprotocol.NewFrame(connectorprotocol.MessageCredentialRotationAck, frame.RequestID, ack)
	if frameErr != nil {
		return nil, frameErr
	}
	return []connectorprotocol.Frame{response}, nil
}

func (s *ControlSession) handleRotationRevoke(ctx context.Context, frame connectorprotocol.Frame) ([]connectorprotocol.Frame, error) {
	var revoke connectorprotocol.CredentialRotationRevoke
	if err := frame.DecodePayload(&revoke); err != nil {
		return nil, err
	}
	if err := s.validateBoundRotation(revoke.AccountID, revoke.TunnelID, revoke.ConnectorID, revoke.HostID, revoke.SessionID, revoke.ProcessGeneration); err != nil {
		return s.rotationError(frame.RequestID, revoke.AccountID, revoke.TunnelID, revoke.OperationID, revoke.ConnectorID, revoke.HostID, revoke.SessionID, revoke.ProcessGeneration, revoke.TargetSetHash, revoke.OldCredentialGeneration, revoke.NewCredentialGeneration, err)
	}
	if s.client.State() != connectorprotocol.SessionReady {
		err := &connectorprotocol.Error{Code: connectorprotocol.CodeCredentialRotationNotReady, Reason: connectorprotocol.ReasonSnapshotRejected, Retryable: true}
		return s.rotationError(frame.RequestID, revoke.AccountID, revoke.TunnelID, revoke.OperationID, revoke.ConnectorID, revoke.HostID, revoke.SessionID, revoke.ProcessGeneration, revoke.TargetSetHash, revoke.OldCredentialGeneration, revoke.NewCredentialGeneration, err)
	}
	if err := s.ensureRotationRecord(ctx, revoke.OperationID, false); err != nil {
		return s.rotationError(frame.RequestID, revoke.AccountID, revoke.TunnelID, revoke.OperationID, revoke.ConnectorID, revoke.HostID, revoke.SessionID, revoke.ProcessGeneration, revoke.TargetSetHash, revoke.OldCredentialGeneration, revoke.NewCredentialGeneration, err)
	}
	if s.rotation == nil {
		return s.rotationError(frame.RequestID, revoke.AccountID, revoke.TunnelID, revoke.OperationID, revoke.ConnectorID, revoke.HostID, revoke.SessionID, revoke.ProcessGeneration, revoke.TargetSetHash, revoke.OldCredentialGeneration, revoke.NewCredentialGeneration, connectorprotocol.ErrUnsupportedMessage)
	}
	ack, err := s.rotation.AcceptRevoke(ctx, revoke)
	if err != nil {
		return s.rotationError(frame.RequestID, revoke.AccountID, revoke.TunnelID, revoke.OperationID, revoke.ConnectorID, revoke.HostID, revoke.SessionID, revoke.ProcessGeneration, revoke.TargetSetHash, revoke.OldCredentialGeneration, revoke.NewCredentialGeneration, err)
	}
	return oneFrame(connectorprotocol.NewFrame(connectorprotocol.MessageCredentialRotationAck, frame.RequestID, ack))
}

func oneFrame(frame connectorprotocol.Frame, err error) ([]connectorprotocol.Frame, error) {
	if err != nil {
		return nil, err
	}
	return []connectorprotocol.Frame{frame}, nil
}

func rotationFailureAck(install connectorprotocol.CredentialRotationInstall, cause error) connectorprotocol.CredentialRotationAck {
	code := connectorprotocol.CodeOf(cause)
	if code == "" || code == connectorprotocol.CodeInvalidInput || code == connectorprotocol.CodeUnsupportedMessage {
		code = connectorprotocol.CodeCredentialRotationFailed
	}
	return connectorprotocol.CredentialRotationAck{
		AccountID: install.AccountID, TunnelID: install.TunnelID, OperationID: install.OperationID,
		ConnectorID: install.ConnectorID, HostID: install.HostID, SessionID: install.SessionID,
		ProcessGeneration: install.ProcessGeneration, TargetSetHash: install.TargetSetHash,
		OldCredentialGeneration: install.OldCredentialGeneration, NewCredentialGeneration: install.NewCredentialGeneration,
		Status: connectorprotocol.RotationAckFailed, Code: code,
	}
}

// rotationFailureFrame preserves a typed, correlated failure for an explicit
// readiness event. The error is returned as well so the supervisor can decide
// whether to retry or mark the operation uncertain; the frame is safe to send
// because it contains only the already-public operation identity and status.
func (s *ControlSession) rotationFailureFrame(requestID string, install connectorprotocol.CredentialRotationInstall, cause error) (connectorprotocol.Frame, error) {
	failure := rotationFailureAck(install, cause)
	frame, frameErr := connectorprotocol.NewFrame(connectorprotocol.MessageCredentialRotationAck, requestID, failure)
	if frameErr != nil {
		return connectorprotocol.Frame{}, errors.Join(cause, frameErr)
	}
	return frame, cause
}

func (s *ControlSession) rotationError(requestID, accountID, tunnelID, operationID, connectorID, hostID, sessionID string, processGeneration uint64, targetSetHash string, oldGeneration, newGeneration uint64, cause error) ([]connectorprotocol.Frame, error) {
	code := connectorprotocol.CodeOf(cause)
	if code == "" || code == connectorprotocol.CodeInvalidInput || code == connectorprotocol.CodeUnsupportedMessage {
		code = connectorprotocol.CodeCredentialRotationRejected
	}
	ack := connectorprotocol.CredentialRotationAck{
		AccountID: accountID, TunnelID: tunnelID, OperationID: operationID, ConnectorID: connectorID,
		HostID: hostID, SessionID: sessionID, ProcessGeneration: processGeneration, TargetSetHash: targetSetHash,
		OldCredentialGeneration: oldGeneration, NewCredentialGeneration: newGeneration,
		Status: connectorprotocol.RotationAckRejected, Code: code,
	}
	response, frameErr := connectorprotocol.NewFrame(connectorprotocol.MessageCredentialRotationAck, requestID, ack)
	if frameErr != nil {
		return nil, errors.Join(cause, frameErr)
	}
	return []connectorprotocol.Frame{response}, cause
}

func (s *ControlSession) applyAck(requestID string, kind connectorprotocol.AckKind, generation uint64, contentHash string, ack connectorprotocol.Ack, applyErr error) ([]connectorprotocol.Frame, error) {
	if ack.Validate() != nil {
		code := connectorprotocol.CodeOf(applyErr)
		if code == "" {
			if kind == connectorprotocol.AckSnapshot {
				code = connectorprotocol.CodeSnapshotRejected
			} else {
				code = connectorprotocol.CodeDeltaRejected
			}
		}
		hello := s.client.Hello()
		s.mu.RLock()
		welcome := s.welcome
		s.mu.RUnlock()
		ack = connectorprotocol.Ack{AccountID: hello.AccountID, TunnelID: hello.TunnelID, ConnectorID: hello.ConnectorID, SessionID: welcome.SessionID, ProcessGeneration: hello.ProcessGeneration, Kind: kind, Status: connectorprotocol.AckRejected, Generation: generation, ContentHash: contentHash, Code: code}
	}
	response, frameErr := connectorprotocol.NewFrame(connectorprotocol.MessageAck, requestID, ack)
	if frameErr != nil {
		return nil, errors.Join(applyErr, frameErr)
	}
	return []connectorprotocol.Frame{response}, applyErr
}

// MarkRotationReadyFrame consumes an already-observed replacement readiness
// event. Keeping this separate from ReplacementReadyFrame lets a process
// supervisor perform a bounded wait without holding the protocol receive loop.
func (s *ControlSession) MarkRotationReadyFrame(ctx context.Context, requestID string, observation ReplacementReadiness) (connectorprotocol.Frame, error) {
	if s == nil || s.client == nil || s.rotation == nil || ctx == nil {
		return connectorprotocol.Frame{}, ErrControlSessionInvalid
	}
	if err := s.requireRotationRuntime(); err != nil {
		return connectorprotocol.Frame{}, err
	}
	record, ok := s.rotation.Record()
	if !ok || record.Install == nil {
		return connectorprotocol.Frame{}, ErrOperationNotFound
	}
	install := *record.Install
	if err := validateReplacementReadiness(observation, install, s.client, s.clock); err != nil {
		return s.rotationFailureFrame(requestID, install, err)
	}
	active, _ := observation.Session.Active()
	identity := observation.Session.Hello()
	ready, err := s.rotation.MarkReady(ctx, identitySessionID(observation.Session), identity.ProcessGeneration, active.Generation, active.ContentHash, observation.EdgeReady, observation.RouteReady, observation.OriginReady)
	if err != nil {
		return s.rotationFailureFrame(requestID, install, err)
	}
	return connectorprotocol.NewFrame(connectorprotocol.MessageCredentialRotationReady, requestID, ready)
}

// ObserveReplacementReady is a convenience for callers that want to own the
// wait and call MarkRotationReadyFrame after their own event scheduling. It
// does not mutate rotation state.
func (s *ControlSession) ObserveReplacementReady(ctx context.Context) (ReplacementReadiness, error) {
	if s == nil || s.rotation == nil || s.readiness == nil || ctx == nil {
		return ReplacementReadiness{}, ErrControlSessionInvalid
	}
	if err := s.requireRotationRuntime(); err != nil {
		return ReplacementReadiness{}, err
	}
	record, ok := s.rotation.Record()
	if !ok || record.Install == nil {
		return ReplacementReadiness{}, ErrOperationNotFound
	}
	return s.readiness.WaitReplacementReady(ctx, *record.Install)
}

// RecoverRotation explicitly restores a durable rotation after a process
// restart. It performs no key generation and emits no frame. Subsequent
// challenge/install/revoke handling remains bound to the newly authenticated
// session through validateBoundRotation.
func (s *ControlSession) RecoverRotation(ctx context.Context, operationID string) error {
	if s == nil || s.client == nil || s.rotation == nil || ctx == nil {
		return ErrControlSessionInvalid
	}
	if err := s.requireRotationRuntime(); err != nil {
		return err
	}
	return s.ensureRotationRecord(ctx, operationID, false)
}
