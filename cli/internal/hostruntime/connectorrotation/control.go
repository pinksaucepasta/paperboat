package connectorrotation

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/connectorprotocol"
)

func NewControlSession(config ControlSessionConfig) (*ControlSession, error) {
	if config.Applier == nil || config.Drainer == nil {
		// ClientSession supplies no-op defaults for unit-level protocol use, but
		// a live host adapter must never claim configuration or drain readiness
		// without the runtime-owned hooks.
		return nil, ErrControlSessionInvalid
	}
	if config.Rotation != nil && config.Readiness == nil {
		// Without an explicit readiness boundary the host could acknowledge an
		// install while no replacement session is serving traffic. Refuse that
		// silent downgrade.
		return nil, ErrControlSessionInvalid
	}
	if config.Rotation != nil && !hasControlCapability(config.Hello.Capabilities, connectorprotocol.CapabilityCredentialRotation) {
		return nil, rotationCapabilityError()
	}
	if hasControlCapability(config.Hello.Capabilities, connectorprotocol.CapabilityCredentialRotation) && config.Rotation == nil {
		return nil, rotationCapabilityError()
	}
	if config.OutboundQueue == 0 {
		config.OutboundQueue = defaultControlOutboundQueue
	}
	if config.OutboundQueue < 1 || config.OutboundQueue > maxControlOutboundQueue {
		return nil, ErrControlSessionInvalid
	}
	if config.RenewalLead == 0 {
		config.RenewalLead = defaultRenewalLead
	}
	if config.RenewalLead <= 0 || config.RenewalLead > connectorprotocol.MaxLease {
		return nil, ErrControlSessionInvalid
	}
	if config.Renewal != nil && config.RenewalSigner != nil {
		return nil, ErrControlSessionInvalid
	}
	if (config.Renewal != nil || config.RenewalSigner != nil) && !hasControlCapability(config.Hello.Capabilities, connectorprotocol.CapabilityRenewal) {
		return nil, &connectorprotocol.Error{Code: connectorprotocol.CodeCapabilityMissing, Reason: connectorprotocol.ReasonCapabilityMissing, Cause: errors.New("credential renewal capability is not negotiated")}
	}
	client, err := connectorprotocol.NewClientSession(connectorprotocol.ClientSessionConfig{
		Hello: config.Hello, Applier: config.Applier, Drainer: config.Drainer,
		Clock: config.Clock, ApplyTimeout: config.ApplyTimeout, AbortTimeout: config.AbortTimeout,
	})
	if err != nil {
		return nil, err
	}
	clock := config.Clock
	if clock == nil {
		clock = controlRealClock{}
	}
	if config.AutomaticRotationReadiness && (config.Rotation == nil || config.Readiness == nil) {
		return nil, ErrControlSessionInvalid
	}
	if config.RotationRevokeCommitter != nil && config.Rotation == nil {
		return nil, ErrControlSessionInvalid
	}
	return &ControlSession{client: client, rotation: config.Rotation, readiness: config.Readiness, snapshotReadiness: config.SnapshotReadiness, renewal: config.Renewal, renewalSigner: config.RenewalSigner, renewalLead: config.RenewalLead, outbound: make(chan connectorprotocol.Frame, config.OutboundQueue), clock: clock, automaticRotation: config.AutomaticRotationReadiness, revokeCommitter: config.RotationRevokeCommitter, rotationReadyOps: make(map[string]struct{})}, nil
}

func (s *ControlSession) Session() *connectorprotocol.ClientSession {
	if s == nil {
		return nil
	}
	return s.client
}

// Welcome returns a copy of the exact negotiated server session after
// authentication. It is safe for generation-bound replacement readiness.
func (s *ControlSession) Welcome() (connectorprotocol.Welcome, bool) {
	if s == nil {
		return connectorprotocol.Welcome{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if !s.welcomed {
		return connectorprotocol.Welcome{}, false
	}
	value := s.welcome
	value.Capabilities = append([]string(nil), value.Capabilities...)
	return value, true
}

// HasSnapshotReadiness reports whether Serve can perform the regular staged
// snapshot cutover. HandleFrame remains available for protocol-level tests and
// rotation events, but a live carrier runner must require this boundary.
func (s *ControlSession) HasSnapshotReadiness() bool {
	return s != nil && s.snapshotReadiness != nil
}

func (s *ControlSession) AcceptWelcomeFrame(frame connectorprotocol.Frame) error {
	if s == nil || s.client == nil || frame.Type != connectorprotocol.MessageWelcome {
		return ErrControlSessionInvalid
	}
	var welcome connectorprotocol.Welcome
	if err := frame.DecodePayload(&welcome); err != nil {
		return err
	}
	if s.rotation != nil && !hasControlCapability(welcome.Capabilities, connectorprotocol.CapabilityCredentialRotation) {
		return rotationCapabilityError()
	}
	if err := s.client.AcceptWelcome(welcome); err != nil {
		return err
	}
	s.mu.Lock()
	s.welcomed = true
	s.welcome = welcome
	s.mu.Unlock()
	return nil
}

// HandleFrame dispatches one server-to-host frame. Rotation install only
// acknowledges that the replacement credential was installed. Replacement
// readiness is an explicit event boundary through MarkRotationReadyFrame so a
// potentially slow process/data-plane wait can never block this receive loop.
func (s *ControlSession) HandleFrame(ctx context.Context, frame connectorprotocol.Frame) ([]connectorprotocol.Frame, error) {
	if s == nil || s.client == nil || ctx == nil {
		return nil, ErrControlSessionInvalid
	}
	if frame.Type == connectorprotocol.MessageWelcome {
		return nil, s.AcceptWelcomeFrame(frame)
	}
	if err := frame.Validate(); err != nil {
		return nil, err
	}
	switch frame.Type {
	case connectorprotocol.MessageAck:
		var ack connectorprotocol.Ack
		if err := frame.DecodePayload(&ack); err != nil {
			return nil, err
		}
		if ack.Kind != connectorprotocol.AckReady {
			return nil, connectorprotocol.ErrUnsupportedMessage
		}
		hello := s.client.Hello()
		s.mu.RLock()
		welcome := s.welcome
		s.mu.RUnlock()
		active, ok := s.client.Active()
		if !ok || ack.AccountID != hello.AccountID || ack.TunnelID != hello.TunnelID || ack.ConnectorID != hello.ConnectorID || ack.SessionID != welcome.SessionID || ack.ProcessGeneration != hello.ProcessGeneration || ack.Generation != active.Generation || ack.ContentHash != active.ContentHash {
			return nil, connectorprotocol.ErrIdentityMismatch
		}
		if ack.Status != connectorprotocol.AckApplied && ack.Status != connectorprotocol.AckDuplicate {
			return nil, connectorprotocol.ErrSnapshotRequired
		}
		return nil, nil
	case connectorprotocol.MessageSnapshot:
		var snapshot connectorprotocol.Snapshot
		if err := frame.DecodePayload(&snapshot); err != nil {
			return nil, err
		}
		ack, applyErr := s.client.ApplySnapshot(ctx, snapshot)
		return s.applyAck(frame.RequestID, connectorprotocol.AckSnapshot, snapshot.Generation, snapshot.ContentHash, ack, applyErr)
	case connectorprotocol.MessageDelta:
		var delta connectorprotocol.Delta
		if err := frame.DecodePayload(&delta); err != nil {
			return nil, err
		}
		ack, applyErr := s.client.ApplyDelta(ctx, delta)
		return s.applyAck(frame.RequestID, connectorprotocol.AckDelta, delta.Generation, delta.ContentHash, ack, applyErr)
	case connectorprotocol.MessageHeartbeatAck:
		var ack connectorprotocol.HeartbeatAck
		if err := frame.DecodePayload(&ack); err != nil {
			return nil, err
		}
		return nil, s.client.AcceptHeartbeatAck(ack)
	case connectorprotocol.MessageAuthRenewed:
		var result connectorprotocol.AuthResult
		if err := frame.DecodePayload(&result); err != nil {
			return nil, err
		}
		return nil, s.client.ApplyRenewal(result)
	case connectorprotocol.MessageDrain:
		var request connectorprotocol.Drain
		if err := frame.DecodePayload(&request); err != nil {
			return nil, err
		}
		ack, drainErr := s.client.HandleDrain(ctx, request)
		response, frameErr := connectorprotocol.NewFrame(connectorprotocol.MessageDrainAck, frame.RequestID, ack)
		if frameErr != nil {
			return nil, errors.Join(drainErr, frameErr)
		}
		return []connectorprotocol.Frame{response}, drainErr
	case connectorprotocol.MessageCredentialRotationChallenge:
		return s.handleRotationChallenge(ctx, frame)
	case connectorprotocol.MessageCredentialRotationInstall:
		return s.handleRotationInstall(ctx, frame)
	case connectorprotocol.MessageCredentialRotationRevoke:
		return s.handleRotationRevoke(ctx, frame)
	case connectorprotocol.MessageCredentialRotationAck, connectorprotocol.MessageCredentialRotationReady:
		return nil, connectorprotocol.ErrUnsupportedMessage
	case connectorprotocol.MessageDisconnect:
		var disconnect connectorprotocol.Disconnect
		if err := frame.DecodePayload(&disconnect); err != nil {
			return nil, err
		}
		return nil, s.client.Close(disconnect.Reason)
	default:
		return nil, connectorprotocol.ErrUnsupportedMessage
	}
}

// Serve sends hello and runs the one framed control loop until the peer closes
// or the session reaches a typed terminal state. Snapshot readiness is waited
// asynchronously so heartbeat, renewal, and rotation frames continue to be
// handled while origins are probed. Only this loop writes frames, which keeps
// wire ordering deterministic and bounds pending readiness to one result.
func (s *ControlSession) Serve(ctx context.Context, carrier io.ReadWriteCloser, helloRequestID string) (serveErr error) {
	if s == nil || carrier == nil || ctx == nil {
		return ErrControlSessionInvalid
	}
	hello, err := s.HelloFrame(helloRequestID)
	if err != nil {
		return err
	}
	if err := connectorprotocol.WriteFrame(carrier, hello); err != nil {
		return err
	}
	runContext, cancel := context.WithCancel(ctx)
	stop := make(chan struct{})
	var closeOnce sync.Once
	var carrierCloseErr error
	closeCarrier := func() {
		closeOnce.Do(func() { carrierCloseErr = carrier.Close() })
	}
	monitorDone := make(chan struct{})
	go func() {
		defer close(monitorDone)
		select {
		case <-runContext.Done():
			closeCarrier()
		case <-stop:
		}
	}()
	defer func() {
		cancel()
		close(stop)
		closeCarrier()
		<-monitorDone
		if carrierCloseErr != nil {
			serveErr = safeRotationFailure("connector control carrier shutdown failed", errors.Join(serveErr, carrierCloseErr))
		}
	}()
	frames := make(chan controlReadResult, 1)
	go readControlFrames(runContext, carrier, frames)
	readyResults := make(chan snapshotReadyResult, 1)
	rotationErrors := make(chan error, 1)
	var pending *snapshotReadyState
	var heartbeatTimer *time.Timer
	var heartbeatEvents <-chan time.Time
	var renewalTimer *time.Timer
	var renewalEvents <-chan time.Time
	renewalPending := false
	heartbeatInterval := time.Duration(0)
	resetHeartbeat := func() {
		if heartbeatInterval <= 0 {
			heartbeatEvents = nil
			return
		}
		if heartbeatTimer == nil {
			heartbeatTimer = time.NewTimer(heartbeatInterval)
		} else {
			resetControlTimer(heartbeatTimer)
			heartbeatTimer.Reset(heartbeatInterval)
		}
		heartbeatEvents = heartbeatTimer.C
	}
	resetRenewal := func() {
		if s.renewal == nil && s.renewalSigner == nil {
			renewalEvents = nil
			return
		}
		if renewalPending {
			renewalEvents = nil
			return
		}
		delay := credentialRenewalDelay(s.clock.Now().UTC(), s.client.Auth().ExpiresAt, s.renewalLead)
		if renewalTimer == nil {
			renewalTimer = time.NewTimer(delay)
		} else {
			resetControlTimer(renewalTimer)
			renewalTimer.Reset(delay)
		}
		renewalEvents = renewalTimer.C
	}
	defer func() {
		if heartbeatTimer != nil {
			heartbeatTimer.Stop()
		}
		if renewalTimer != nil {
			renewalTimer.Stop()
		}
		stopSnapshotReadiness(pending)
	}()
	for {
		select {
		case <-runContext.Done():
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return runContext.Err()
		case result := <-frames:
			if result.err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				return result.err
			}
			responses, handleErr := s.HandleFrame(runContext, result.frame)
			var revoke *connectorprotocol.CredentialRotationRevoke
			if result.frame.Type == connectorprotocol.MessageCredentialRotationRevoke && handleErr == nil && s.revokeCommitter != nil {
				var value connectorprotocol.CredentialRotationRevoke
				if err := result.frame.DecodePayload(&value); err != nil {
					return err
				}
				revoke = &value
				if preparer, ok := s.revokeCommitter.(RotationRevokePreparer); ok {
					if err := preparer.PrepareRevoke(runContext, value); err != nil {
						return err
					}
				}
			}
			for _, response := range responses {
				if err := connectorprotocol.WriteFrame(carrier, response); err != nil {
					return err
				}
			}
			if handleErr != nil {
				return handleErr
			}
			if result.frame.Type == connectorprotocol.MessageCredentialRotationInstall && s.automaticRotation {
				var install connectorprotocol.CredentialRotationInstall
				if err := result.frame.DecodePayload(&install); err != nil {
					return err
				}
				s.startAutomaticRotationReady(runContext, install, rotationErrors)
			}
			if revoke != nil {
				if err := s.revokeCommitter.CommitRevoke(runContext, *revoke); err != nil {
					return err
				}
				if rejoiner, ok := s.revokeCommitter.(RotationRevokeRejoiner); ok && rejoiner.RejoinAfterRevoke() {
					return nil
				}
			}
			if result.frame.Type == connectorprotocol.MessageWelcome {
				s.mu.RLock()
				welcome := s.welcome
				s.mu.RUnlock()
				heartbeatInterval = time.Duration(welcome.Lease.HeartbeatIntervalMS) * time.Millisecond
				renewalPending = false
				resetHeartbeat()
				resetRenewal()
			} else if result.frame.Type == connectorprotocol.MessageAuthRenewed {
				renewalPending = false
				resetRenewal()
			}
			if applied, ok := appliedSnapshot(result.frame, responses); ok {
				if s.snapshotReadiness == nil {
					return ErrControlSessionInvalid
				}
				candidate, candidateOK := s.client.Candidate()
				if !candidateOK || candidate.Generation != applied.Generation || candidate.ContentHash != applied.ContentHash {
					return ErrControlSessionInvalid
				}
				applied = candidate
				if pending != nil {
					pending.cancel()
				}
				readinessContext, readinessCancel := context.WithCancel(runContext)
				state := &snapshotReadyState{snapshot: applied, requestID: result.frame.RequestID, cancel: readinessCancel, done: make(chan struct{})}
				pending = state
				go s.waitSnapshotReady(readinessContext, state, readyResults)
			}
			if s.client.State() == connectorprotocol.SessionClosed {
				return nil
			}
		case outgoing := <-s.outbound:
			if err := connectorprotocol.WriteFrame(carrier, outgoing); err != nil {
				return err
			}
			if outgoing.Type == connectorprotocol.MessageAuthRenew {
				renewalPending = true
				if renewalTimer != nil {
					resetControlTimer(renewalTimer)
				}
				renewalEvents = nil
			}
		case <-heartbeatEvents:
			// Heartbeat uses the promoted generation when one exists, and the
			// exact staged candidate during bootstrap. The server accepts the
			// latter only as a lease renewal; it never promotes readiness from a
			// heartbeat. This keeps a slow carrier/origin probe from expiring the
			// session that is waiting to become ready.
			frame, err := s.HeartbeatFrame(s.nextRequestID("heartbeat"), s.clock.Now().UTC())
			if err != nil {
				if !errors.Is(err, connectorprotocol.ErrSnapshotRequired) && !errors.Is(err, connectorprotocol.ErrNotReady) {
					return err
				}
			} else if err := connectorprotocol.WriteFrame(carrier, frame); err != nil {
				return err
			}
			resetHeartbeat()
		case <-renewalEvents:
			if s.renewal == nil && s.renewalSigner == nil {
				renewalEvents = nil
				continue
			}
			now := s.clock.Now().UTC()
			var frame connectorprotocol.Frame
			var err error
			if s.renewalSigner != nil {
				frame, err = s.signedRenewalFrame(runContext, s.nextRequestID("renewal"), now)
			} else {
				nonce, signedProof, renewalErr := s.renewal.Renew(runContext, now)
				if renewalErr != nil {
					return renewalErr
				}
				frame, err = s.RenewalFrame(s.nextRequestID("renewal"), now, nonce, signedProof)
			}
			if err != nil {
				return err
			}
			if err := connectorprotocol.WriteFrame(carrier, frame); err != nil {
				return err
			}
			renewalPending = true
			renewalEvents = nil
		case result := <-readyResults:
			if pending == nil || !pending.matches(result) {
				// A canceled readiness worker may race with a newer candidate. The
				// key check fences its result without affecting session state.
				continue
			}
			pending.cancel()
			pending = nil
			if result.err != nil {
				return result.err
			}
			if err := validateSnapshotReadiness(result.snapshot, result.readiness); err != nil {
				return err
			}
			readiness, err := s.client.MarkReadyContext(runContext, true, true, true)
			if err != nil {
				return err
			}
			if err := validateSnapshotReadiness(result.snapshot, readiness); err != nil {
				return err
			}
			readyFrame, err := connectorprotocol.NewFrame(connectorprotocol.MessageReady, result.requestID, readiness)
			if err != nil {
				return err
			}
			if err := connectorprotocol.WriteFrame(carrier, readyFrame); err != nil {
				return err
			}
		case rotationErr := <-rotationErrors:
			if rotationErr != nil {
				return rotationErr
			}
		}
	}
}

func (s *ControlSession) startAutomaticRotationReady(ctx context.Context, install connectorprotocol.CredentialRotationInstall, results chan<- error) {
	if s == nil || ctx == nil || results == nil {
		return
	}
	s.mu.Lock()
	if _, exists := s.rotationReadyOps[install.OperationID]; exists {
		s.mu.Unlock()
		return
	}
	s.rotationReadyOps[install.OperationID] = struct{}{}
	s.mu.Unlock()
	go func() {
		frame, err := s.ReplacementReadyFrame(ctx, s.nextRequestID("rotation-ready"))
		if err == nil {
			err = s.EnqueueFrame(frame)
		}
		if err != nil {
			select {
			case results <- err:
			case <-ctx.Done():
			}
		}
	}()
}
