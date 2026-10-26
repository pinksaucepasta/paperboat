package connectorprotocol

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"

	wire "github.com/pinksaucepasta/paperboat/shared/connectorprotocol"
)

// ActiveControlSession is the secret-free server projection of one currently
// authenticated connector control stream. It is used only to bind follow-up
// carrier bootstrap reads to the exact session created by Welcome.
type ActiveControlSession struct {
	AccountID             string
	TunnelID              string
	ConnectorID           string
	HostID                string
	SessionID             string
	IdentityKeyID         string
	IdentityKeyThumbprint string
	ProcessGeneration     uint64
	CredentialGeneration  uint64
	ConfigGeneration      uint64
	ConfigContentHash     string
	// Serving is the config the session reported ready; Candidate the config it acknowledged and
	// is preparing carriers for. Both are filled by Lookup.
	ServingConfigGeneration    uint64
	ServingConfigContentHash   string
	CandidateConfigGeneration  uint64
	CandidateConfigContentHash string
}

// Serves reports whether a connector request for this config belongs to the session: the config
// it was welcomed with, the one it serves, or the acknowledged candidate it is preparing. A config
// edit therefore never needs a new control session (it used to answer connector_session_stale
// and make the connector reconnect, re-generating the tunnel's route on every edge).
func (s ActiveControlSession) Serves(generation uint64, contentHash string) bool {
	return generation == s.ConfigGeneration && contentHash == s.ConfigContentHash ||
		generation == s.ServingConfigGeneration && contentHash == s.ServingConfigContentHash ||
		generation == s.CandidateConfigGeneration && contentHash == s.CandidateConfigContentHash
}

func (s ActiveControlSession) validate() error {
	if wire.ValidateIdentifier(s.AccountID) != nil || wire.ValidateIdentifier(s.TunnelID) != nil || wire.ValidateIdentifier(s.ConnectorID) != nil || wire.ValidateIdentifier(s.HostID) != nil || wire.ValidateIdentifier(s.SessionID) != nil || wire.ValidateIdentityKey(s.IdentityKeyID, s.IdentityKeyThumbprint) != nil || s.ProcessGeneration == 0 || s.CredentialGeneration == 0 || s.ConfigGeneration == 0 || !wire.ValidContentHash(s.ConfigContentHash) {
		return wire.ErrInvalidInput
	}
	return nil
}

// ActiveControlSessions owns no stream. Generation-safe detach prevents an
// older connection finishing after replacement from deleting the new entry.
type ActiveControlSessions struct {
	mu       sync.RWMutex
	sessions map[string]activeControlSessionEntry
}

type activeControlSessionEntry struct {
	projection ActiveControlSession
	session    *PersistentSession
}

func NewActiveControlSessions() *ActiveControlSessions {
	return &ActiveControlSessions{sessions: make(map[string]activeControlSessionEntry)}
}

func (s *ActiveControlSessions) attach(projection ActiveControlSession, session *PersistentSession) error {
	if s == nil || session == nil || projection.validate() != nil {
		return wire.ErrInvalidInput
	}
	key := controlSessionKey(projection.TunnelID, projection.ConnectorID)
	s.mu.Lock()
	defer s.mu.Unlock()
	if current, ok := s.sessions[key]; ok {
		if current.projection.ProcessGeneration > projection.ProcessGeneration || current.projection.ProcessGeneration == projection.ProcessGeneration && current.projection.SessionID != projection.SessionID {
			return wire.CodeError(wire.ErrSessionConflict, wire.ReasonSessionReplaced, true, nil)
		}
	}
	s.sessions[key] = activeControlSessionEntry{projection: projection, session: session}
	return nil
}

func (s *ActiveControlSessions) detach(projection ActiveControlSession) {
	if s == nil {
		return
	}
	key := controlSessionKey(projection.TunnelID, projection.ConnectorID)
	s.mu.Lock()
	defer s.mu.Unlock()
	current, ok := s.sessions[key]
	if ok && current.projection.SessionID == projection.SessionID && current.projection.ProcessGeneration == projection.ProcessGeneration {
		delete(s.sessions, key)
	}
}

// Lookup returns a copy only when every session fence matches.
func (s *ActiveControlSessions) Lookup(tunnelID, connectorID, sessionID string, processGeneration uint64) (ActiveControlSession, bool) {
	if s == nil || wire.ValidateIdentifier(tunnelID) != nil || wire.ValidateIdentifier(connectorID) != nil || wire.ValidateIdentifier(sessionID) != nil || processGeneration == 0 {
		return ActiveControlSession{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	current, ok := s.sessions[controlSessionKey(tunnelID, connectorID)]
	if !ok || current.projection.SessionID != sessionID || current.projection.ProcessGeneration != processGeneration {
		return ActiveControlSession{}, false
	}
	projection := current.projection
	if snapshot, ready, _ := current.session.Current(); ready {
		projection.ServingConfigGeneration, projection.ServingConfigContentHash = snapshot.Generation, snapshot.ContentHash
	}
	if candidate, ok := current.session.Candidate(); ok {
		projection.CandidateConfigGeneration, projection.CandidateConfigContentHash = candidate.Generation, candidate.ContentHash
	}
	return projection, true
}

func controlSessionKey(tunnelID, connectorID string) string { return tunnelID + "\x00" + connectorID }

// ControlTerminationKind identifies why the server stopped serving one
// connector control stream. It deliberately describes the transport boundary
// only: payloads and raw errors are never included in the event.
type ControlTerminationKind string

const (
	ControlTerminationUnknown         ControlTerminationKind = "unknown"
	ControlTerminationInvalidRequest  ControlTerminationKind = "invalid_request"
	ControlTerminationPeerEOF         ControlTerminationKind = "peer_eof"
	ControlTerminationPeerCanceled    ControlTerminationKind = "peer_canceled"
	ControlTerminationPeerDisconnect  ControlTerminationKind = "peer_disconnect"
	ControlTerminationPeerReadError   ControlTerminationKind = "peer_read_error"
	ControlTerminationPeerWriteError  ControlTerminationKind = "peer_write_error"
	ControlTerminationAuthentication  ControlTerminationKind = "authentication_error"
	ControlTerminationSessionExpired  ControlTerminationKind = "session_expired"
	ControlTerminationSessionReplaced ControlTerminationKind = "session_replaced"
	ControlTerminationProtocolError   ControlTerminationKind = "protocol_error"
	ControlTerminationServerShutdown  ControlTerminationKind = "server_shutdown"
	ControlTerminationServerError     ControlTerminationKind = "server_error"
)

// ControlTerminationEvent is a bounded, secret-free diagnostic projection of
// one served stream. The observer is called after the session cleanup path has
// run, so SessionID and the protocol disconnect reason are stable.
type ControlTerminationEvent struct {
	TunnelID          string
	ConnectorID       string
	SessionID         string
	ProcessGeneration uint64
	Kind              ControlTerminationKind
	ProtocolReason    wire.DisconnectReason
	Code              wire.Code
	Retryable         bool
}

type ControlTransport struct {
	server   *PersistentServer
	sessions *ActiveControlSessions
	rotation *RotationDispatcher
	drain    *DrainDispatcher

	terminationMu       sync.RWMutex
	terminationObserver func(ControlTerminationEvent)
}

func NewControlTransport(server *PersistentServer, sessions *ActiveControlSessions) (*ControlTransport, error) {
	return NewControlTransportWithRotation(server, sessions, nil)
}

func NewControlTransportWithRotation(server *PersistentServer, sessions *ActiveControlSessions, rotation *RotationDispatcher) (*ControlTransport, error) {
	return NewControlTransportWithDispatchers(server, sessions, rotation, nil)
}

func NewControlTransportWithDispatchers(server *PersistentServer, sessions *ActiveControlSessions, rotation *RotationDispatcher, drain *DrainDispatcher) (*ControlTransport, error) {
	if server == nil {
		return nil, wire.ErrInvalidInput
	}
	if sessions == nil {
		sessions = NewActiveControlSessions()
	}
	return &ControlTransport{server: server, sessions: sessions, rotation: rotation, drain: drain}, nil
}

func (t *ControlTransport) Sessions() *ActiveControlSessions {
	if t == nil {
		return nil
	}
	return t.sessions
}

// SetTerminationObserver installs an optional bounded diagnostic callback. It
// is intended to be set during application construction, before Serve is
// called. HTTP owners report failures with request correlation; this
// transport does not emit a second process-global log record.
func (t *ControlTransport) SetTerminationObserver(observer func(ControlTerminationEvent)) {
	if t == nil {
		return
	}
	t.terminationMu.Lock()
	t.terminationObserver = observer
	t.terminationMu.Unlock()
}

func (t *ControlTransport) reportTermination(event ControlTerminationEvent) {
	if t == nil {
		return
	}
	t.terminationMu.RLock()
	observer := t.terminationObserver
	t.terminationMu.RUnlock()
	if observer != nil {
		observer(event)
	}
}

func populateTermination(event *ControlTerminationEvent, cause error) {
	if event == nil {
		return
	}
	event.Code = wire.CodeOf(cause)
	event.ProtocolReason = wire.ReasonOf(cause)
	var typed *wire.Error
	if errors.As(cause, &typed) && typed != nil {
		event.Retryable = typed.Retryable
	}
}

func classifyControlError(cause error) ControlTerminationKind {
	switch wire.ReasonOf(cause) {
	case wire.ReasonAuthentication:
		return ControlTerminationAuthentication
	case wire.ReasonCredentialExpired, wire.ReasonLeaseExpired, wire.ReasonHeartbeatTimeout:
		return ControlTerminationSessionExpired
	case wire.ReasonSessionReplaced, wire.ReasonStaleGeneration:
		return ControlTerminationSessionReplaced
	case wire.ReasonServerShutdown:
		return ControlTerminationServerShutdown
	default:
		return ControlTerminationProtocolError
	}
}

func classifyReadTermination(ctx context.Context, cause error) ControlTerminationKind {
	if ctx != nil && ctx.Err() != nil {
		return ControlTerminationServerShutdown
	}
	if errors.Is(cause, io.EOF) {
		return ControlTerminationPeerEOF
	}
	if errors.Is(cause, context.Canceled) {
		return ControlTerminationPeerCanceled
	}
	return ControlTerminationPeerReadError
}

func classifyWriteTermination(ctx context.Context) ControlTerminationKind {
	if ctx != nil && ctx.Err() != nil {
		return ControlTerminationServerShutdown
	}
	return ControlTerminationPeerWriteError
}

// Serve runs one connector-v1 control stream. The first frame must be Hello;
// the path-provided tunnel and connector are checked before durable auth. All
// writes share one bounded serializer so responses and asynchronous rotation
// frames cannot interleave on the byte stream.
func (t *ControlTransport) Serve(ctx context.Context, stream io.ReadWriteCloser, expectedTunnelID, expectedConnectorID string) (serveErr error) {
	event := ControlTerminationEvent{
		TunnelID: expectedTunnelID, ConnectorID: expectedConnectorID,
		Kind: ControlTerminationUnknown,
	}
	finish := func(kind ControlTerminationKind, cause error) error {
		event.Kind = kind
		if kind != ControlTerminationPeerEOF && kind != ControlTerminationPeerCanceled {
			populateTermination(&event, cause)
		}
		return cause
	}
	defer func() {
		if event.Kind == ControlTerminationUnknown {
			event.Kind = ControlTerminationServerError
			populateTermination(&event, serveErr)
		}
		t.reportTermination(event)
	}()
	if t == nil || t.server == nil || t.sessions == nil || ctx == nil || stream == nil || wire.ValidateIdentifier(expectedTunnelID) != nil || wire.ValidateIdentifier(expectedConnectorID) != nil {
		return finish(ControlTerminationInvalidRequest, wire.ErrInvalidInput)
	}
	defer stream.Close()
	streamDone := make(chan struct{})
	defer close(streamDone)
	go func() {
		select {
		case <-ctx.Done():
			// ReadFrame may be blocked in a WebSocket or net.Pipe read. Closing
			// the stream here makes server shutdown/cancellation interrupt that
			// read instead of leaking the handler until the peer notices.
			_ = stream.Close()
		case <-streamDone:
		}
	}()
	writer := newSerializedFrameWriter(stream)
	first, err := wire.ReadFrame(stream)
	if err != nil {
		kind := classifyReadTermination(ctx, err)
		if kind == ControlTerminationPeerEOF || kind == ControlTerminationPeerCanceled || kind == ControlTerminationServerShutdown {
			_ = finish(kind, err)
			return nil
		}
		return finish(kind, err)
	}
	if first.Type != wire.MessageHello {
		err = wire.CodeError(wire.ErrUnsupportedMessage, wire.ReasonProtocolMismatch, false, nil)
		return finish(ControlTerminationProtocolError, err)
	}
	var hello wire.Hello
	if err = first.DecodePayload(&hello); err != nil {
		return finish(ControlTerminationProtocolError, err)
	}
	if hello.TunnelID != expectedTunnelID || hello.ConnectorID != expectedConnectorID {
		err = wire.CodeError(wire.ErrIdentityMismatch, wire.ReasonAuthentication, false, nil)
		return finish(ControlTerminationAuthentication, err)
	}
	session, welcome, snapshot, err := t.server.Accept(ctx, hello)
	if err != nil {
		return finish(classifyControlError(err), err)
	}
	projection := ActiveControlSession{
		AccountID: hello.AccountID, TunnelID: hello.TunnelID, ConnectorID: hello.ConnectorID,
		HostID: hello.HostID, SessionID: welcome.SessionID, IdentityKeyID: hello.Auth.IdentityKeyID,
		IdentityKeyThumbprint: hello.Auth.IdentityKeyThumbprint, ProcessGeneration: hello.ProcessGeneration,
		CredentialGeneration: hello.Auth.CredentialGeneration, ConfigGeneration: snapshot.Generation,
		ConfigContentHash: snapshot.ContentHash,
	}
	event.SessionID = welcome.SessionID
	event.ProcessGeneration = hello.ProcessGeneration
	if err = t.sessions.attach(projection, session); err != nil {
		_ = session.Close(context.Background(), wire.ReasonSessionReplaced)
		return finish(ControlTerminationSessionReplaced, err)
	}
	defer t.sessions.detach(projection)
	closeReason := wire.ReasonProtocolClosed
	defer func() {
		if ctx.Err() != nil {
			closeReason = wire.ReasonServerShutdown
		}
		closeErr := session.Close(context.Background(), closeReason)
		if serveErr == nil && closeErr != nil {
			serveErr = closeErr
		}
	}()

	welcomeFrame, err := wire.NewFrame(wire.MessageWelcome, first.RequestID, welcome)
	if err != nil {
		return finish(ControlTerminationServerError, errors.Join(err, errors.New("server could not encode connector welcome")))
	}
	if err = writer.Send(ctx, welcomeFrame); err != nil {
		return finish(classifyWriteTermination(ctx), err)
	}
	snapshotRequestID, err := wire.NewOpaqueID("request")
	if err != nil {
		return finish(ControlTerminationServerError, err)
	}
	snapshotFrame, err := wire.NewFrame(wire.MessageSnapshot, snapshotRequestID, snapshot)
	if err != nil {
		return finish(ControlTerminationServerError, errors.Join(err, errors.New("server could not encode connector snapshot")))
	}
	if err = writer.Send(ctx, snapshotFrame); err != nil {
		return finish(classifyWriteTermination(ctx), err)
	}
	if t.rotation != nil {
		live := RotationLiveSession{
			AccountID: hello.AccountID, HostID: hello.HostID, Reference: session.Reference(),
			CredentialGeneration: hello.Auth.CredentialGeneration,
			IdentityKeyID:        hello.Auth.IdentityKeyID, IdentityKeyThumbprint: hello.Auth.IdentityKeyThumbprint,
			NegotiatedCapabilities: append([]string(nil), welcome.Capabilities...), Send: writer.Send,
		}
		if err = t.rotation.RegisterSession(ctx, live); err != nil {
			return finish(ControlTerminationServerError, err)
		}
		defer t.rotation.DetachSession(session.Reference())
	}
	if t.drain != nil {
		live := DrainLiveSession{
			Projection: projection, Session: session,
			NegotiatedCapabilities: append([]string(nil), welcome.Capabilities...), Send: writer.Send,
		}
		if err = t.drain.RegisterSession(ctx, live); err != nil {
			return finish(ControlTerminationServerError, err)
		}
		defer t.drain.DetachSession(session.Reference())
	}

	lastOffered := snapshot
	candidateAcknowledged := false
	for {
		frame, readErr := wire.ReadFrame(stream)
		if readErr != nil {
			if ctx.Err() != nil {
				_ = finish(ControlTerminationServerShutdown, ctx.Err())
				return nil
			}
			kind := classifyReadTermination(ctx, readErr)
			if kind == ControlTerminationPeerEOF || kind == ControlTerminationPeerCanceled {
				_ = finish(kind, readErr)
				return nil
			}
			return finish(kind, readErr)
		}
		response, terminal, handleErr := handleControlFrame(ctx, session, t.rotation, frame)
		if handleErr != nil {
			if rejectErr := writeControlReject(ctx, writer, session.Reference(), hello.AccountID, frame.RequestID, handleErr); rejectErr != nil {
				handleErr = errors.Join(handleErr, rejectErr, errors.New("write connector control rejection failed"))
			}
			return finish(classifyControlError(handleErr), handleErr)
		}
		if frame.Type == wire.MessageAck {
			var ack wire.Ack
			if frame.DecodePayload(&ack) == nil && ack.Status == wire.AckApplied {
				// HandleAck persisted this exact candidate before bootstrap can
				// read it; old serving readiness remains owned by the session.
				candidateAcknowledged = true
				projection.ConfigGeneration, projection.ConfigContentHash = ack.Generation, ack.ContentHash
				if err = t.sessions.attach(projection, session); err != nil {
					return finish(classifyControlError(err), err)
				}
			}
		}
		if response != nil {
			if err = writer.Send(ctx, *response); err != nil {
				return finish(classifyWriteTermination(ctx), err)
			}
		}
		if frame.Type == wire.MessageHeartbeat {
			if !candidateAcknowledged {
				// Wait for the exact offered ACK. Once durable, a newer desired
				// snapshot can replace an unready candidate without losing LKG.
				continue
			}
			// The existing heartbeat cadence drives desired configuration;
			// long-lived paused sessions must receive resume without reconnect.
			// A heartbeat reads only the newest generation and hash; the payload is read and parsed only
			// when the generation advanced (F21-22).
			fetchCtx, cancel := context.WithTimeout(ctx, wire.DefaultAbortTimeout)
			generation, hash, fetchErr := t.server.store.SnapshotVersion(fetchCtx, hello.TunnelID)
			var desired wire.Snapshot
			if fetchErr == nil {
				desired = wire.Snapshot{TunnelID: hello.TunnelID, Generation: generation, ContentHash: hash}
				if generation > lastOffered.Generation {
					desired, fetchErr = t.server.store.Snapshot(fetchCtx, hello.TunnelID)
				}
			}
			cancel()
			if fetchErr != nil {
				return finish(classifyControlError(fetchErr), fetchErr)
			}
			if desired.Generation == lastOffered.Generation && desired.ContentHash != lastOffered.ContentHash {
				return finish(ControlTerminationServerError, errors.Join(wire.ErrContentHashMismatch, errors.New("stored connector snapshot changed without a generation advance")))
			}
			if desired.Generation > lastOffered.Generation {
				desired = wire.BindSnapshot(desired, hello.AccountID, hello.ConnectorID, welcome.SessionID, hello.ProcessGeneration)
				if err = session.OfferSnapshot(ctx, desired); err != nil {
					return finish(classifyControlError(err), err)
				}
				requestID, idErr := wire.NewOpaqueID("request")
				if idErr != nil {
					return finish(ControlTerminationServerError, idErr)
				}
				update, frameErr := wire.NewFrame(wire.MessageSnapshot, requestID, desired)
				if frameErr != nil {
					return finish(ControlTerminationServerError, errors.Join(frameErr, errors.New("server could not encode updated connector snapshot")))
				}
				if err = writer.Send(ctx, update); err != nil {
					return finish(classifyWriteTermination(ctx), err)
				}
				lastOffered = desired
				candidateAcknowledged = false
			}
		}
		if terminal {
			if frame.Type == wire.MessageDisconnect {
				var disconnect wire.Disconnect
				if frame.DecodePayload(&disconnect) == nil {
					closeReason = disconnect.Reason
				}
			}
			if frame.Type == wire.MessageDisconnect {
				event.Kind = ControlTerminationPeerDisconnect
				event.ProtocolReason = closeReason
			}
			return nil
		}
	}
}

func handleControlFrame(ctx context.Context, session *PersistentSession, rotation *RotationDispatcher, frame wire.Frame) (*wire.Frame, bool, error) {
	if ctx == nil || session == nil {
		return nil, false, wire.ErrInvalidInput
	}
	switch frame.Type {
	case wire.MessageAck:
		var value wire.Ack
		if err := frame.DecodePayload(&value); err != nil {
			return nil, false, err
		}
		return nil, false, session.HandleAck(ctx, value)
	case wire.MessageReady:
		var value wire.Readiness
		if err := frame.DecodePayload(&value); err != nil {
			return nil, false, err
		}
		ack, err := session.HandleReadiness(ctx, value)
		if err != nil {
			return nil, false, err
		}
		response, err := wire.NewFrame(wire.MessageAck, frame.RequestID, ack)
		if err != nil {
			return nil, false, errors.Join(err, errors.New("server could not encode readiness acknowledgement"))
		}
		return &response, false, err
	case wire.MessageHeartbeat:
		var value wire.Heartbeat
		if err := frame.DecodePayload(&value); err != nil {
			return nil, false, err
		}
		ack, err := session.HandleHeartbeat(ctx, value)
		if err != nil {
			return nil, false, err
		}
		response, err := wire.NewFrame(wire.MessageHeartbeatAck, frame.RequestID, ack)
		if err != nil {
			return nil, false, errors.Join(err, errors.New("server could not encode heartbeat acknowledgement"))
		}
		return &response, false, err
	case wire.MessageAuthRenew:
		var value wire.RenewalRequest
		if err := frame.DecodePayload(&value); err != nil {
			return nil, false, err
		}
		result, err := session.Renew(ctx, value)
		if err != nil {
			return nil, false, err
		}
		response, err := wire.NewFrame(wire.MessageAuthRenewed, frame.RequestID, result)
		if err != nil {
			return nil, false, errors.Join(err, errors.New("server could not encode credential renewal response"))
		}
		return &response, false, err
	case wire.MessageDrainAck:
		var value wire.DrainAck
		if err := frame.DecodePayload(&value); err != nil {
			return nil, false, err
		}
		return nil, false, session.HandleDrainAck(ctx, value)
	case wire.MessageCredentialRotationProof, wire.MessageCredentialRotationReady, wire.MessageCredentialRotationAck:
		if rotation == nil {
			return nil, false, wire.CodeError(wire.ErrUnsupportedMessage, wire.ReasonCredentialRotation, false, nil)
		}
		return nil, false, rotation.HandleFrame(ctx, session.Reference(), frame)
	case wire.MessageDisconnect:
		var value wire.Disconnect
		if err := frame.DecodePayload(&value); err != nil {
			return nil, false, err
		}
		ref := session.Reference()
		if value.TunnelID != ref.TunnelID || value.ConnectorID != ref.ConnectorID || value.SessionID != ref.SessionID || value.ProcessGeneration != ref.ProcessGeneration {
			return nil, false, wire.CodeError(wire.ErrIdentityMismatch, wire.ReasonAuthentication, false, nil)
		}
		return nil, true, nil
	default:
		return nil, false, wire.CodeError(wire.ErrUnsupportedMessage, wire.ReasonProtocolMismatch, false, nil)
	}
}

type serializedFrameWriter struct {
	writer io.Writer
	gate   chan struct{}
}

func newSerializedFrameWriter(writer io.Writer) *serializedFrameWriter {
	value := &serializedFrameWriter{writer: writer, gate: make(chan struct{}, 1)}
	value.gate <- struct{}{}
	return value
}

func (w *serializedFrameWriter) Send(ctx context.Context, frame wire.Frame) error {
	if w == nil || w.writer == nil || ctx == nil {
		return wire.ErrInvalidInput
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-w.gate:
	}
	defer func() { w.gate <- struct{}{} }()
	if deadline, ok := ctx.Deadline(); ok {
		if connection, ok := w.writer.(interface{ SetWriteDeadline(time.Time) error }); ok {
			if err := connection.SetWriteDeadline(deadline); err != nil {
				return err
			}
			defer connection.SetWriteDeadline(time.Time{})
		}
	}
	return wire.WriteFrame(w.writer, frame)
}

func writeControlReject(ctx context.Context, writer *serializedFrameWriter, ref wire.SessionRef, accountID, requestID string, cause error) error {
	code := wire.CodeOf(cause)
	if code == "" {
		code = wire.CodeSessionClosed
	}
	reason := wire.ReasonOf(cause)
	if reason == "" {
		reason = wire.ReasonProtocolClosed
	}
	retryable := false
	var typed *wire.Error
	if errors.As(cause, &typed) {
		retryable = typed.Retryable
	}
	reject := wire.Reject{AccountID: accountID, TunnelID: ref.TunnelID, ConnectorID: ref.ConnectorID, SessionID: ref.SessionID, ProcessGeneration: ref.ProcessGeneration, Code: code, Reason: reason, Retryable: retryable}
	frame, err := wire.NewFrame(wire.MessageReject, requestID, reject)
	if err != nil {
		return err
	}
	return writer.Send(ctx, frame)
}
