package connectorprotocol

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	wire "github.com/pinksaucepasta/paperboat/shared/connectorprotocol"

	servertelemetry "github.com/pinksaucepasta/paperboat-server/internal/telemetry"
)

// PersistentControlStore is the server adapter boundary for migration 122's
// connector, connector-session, and config-generation records. Implementations
// own database transactions and secret custody. The protocol passes only
// metadata and detached proofs, never bearer or private credential bytes.
//
// AuthenticateConnector and RenewConnector must perform durable nonce replay
// protection in the same transaction as the authorization decision. A process
// local replay cache is deliberately not part of this contract: it cannot
// protect a second server replica or a process restart.
type PersistentControlStore interface {
	AuthenticateConnector(context.Context, wire.AuthRequest) (wire.AuthResult, error)
	RenewConnector(context.Context, wire.RenewalRequest) (wire.AuthResult, error)
	Snapshot(context.Context, string) (wire.Snapshot, error)
	CreateConnectorSession(context.Context, wire.SessionRef, wire.Lease, uint64) error
	RecordApplied(context.Context, wire.SessionRef, wire.Ack) error
	RecordReady(context.Context, wire.SessionRef, wire.Readiness) error
	RecordHeartbeat(context.Context, wire.SessionRef, wire.Heartbeat, wire.HeartbeatAck) error
	RecordRenewal(context.Context, wire.SessionRef, wire.AuthResult) error
	RecordDisconnected(context.Context, wire.SessionRef, wire.Disconnect) error
}

// PersistentDrainStore is required by NewPersistentServer. Drain transitions
// change admission eligibility and therefore cannot silently fall back to
// process-local state. Keeping the lifecycle separate lets TRK-07's operation
// repository add its own operation/audit event in the same transaction without
// widening the wire package's core store contract.
type PersistentDrainStore interface {
	RecordDrain(context.Context, wire.SessionRef, wire.Drain, wire.DrainStatus, uint32, wire.Code) error
}

// PersistentSessionMetadataStore lets the SQL adapter persist the negotiated
// version and capabilities without putting those fields into the smaller
// legacy-shaped CreateConnectorSession method.
type PersistentSessionMetadataStore interface {
	CreateConnectorSessionV1(context.Context, wire.SessionRef, wire.Welcome, uint64) error
}

// RotationPlanSource is the restart/reconciliation boundary. Implementations
// return plans reconstructed from immutable target rows, not from a fresh
// connector listing.
type RotationPlanSource interface {
	ListCredentialRotationPlans(context.Context, int) ([]wire.RotationPlan, error)
}

// IdentityProofVerifier resolves the enrolled public key using the request's
// account/tunnel/connector/host/key identity and verifies the exact transcript
// supplied by this package. A verifier must do its lookup against durable
// enrollment state; a single global callback is unsafe for a multi-host server.
type IdentityProofVerifier interface {
	VerifyAuthProof(context.Context, wire.AuthRequest, []byte, []byte) error
	VerifyRenewalProof(context.Context, wire.RenewalRequest, []byte, []byte) error
}

// IdentityProofVerifierFuncs is convenient for tests and for an adapter that
// delegates key lookup to an existing identity service.
type IdentityProofVerifierFuncs struct {
	AuthFunc    func(context.Context, wire.AuthRequest, []byte, []byte) error
	RenewalFunc func(context.Context, wire.RenewalRequest, []byte, []byte) error
}

func (f IdentityProofVerifierFuncs) VerifyAuthProof(ctx context.Context, request wire.AuthRequest, payload, signature []byte) error {
	if f.AuthFunc == nil {
		return errors.New("authentication proof verifier is not configured")
	}
	return f.AuthFunc(ctx, request, payload, signature)
}

func (f IdentityProofVerifierFuncs) VerifyRenewalProof(ctx context.Context, request wire.RenewalRequest, payload, signature []byte) error {
	if f.RenewalFunc == nil {
		return errors.New("renewal proof verifier is not configured")
	}
	return f.RenewalFunc(ctx, request, payload, signature)
}

type PersistentAuthenticator struct {
	Store    PersistentControlStore
	Verifier IdentityProofVerifier
	Clock    wire.Clock
}

func (a PersistentAuthenticator) now() time.Time {
	if a.Clock == nil {
		return time.Now().UTC()
	}
	return a.Clock.Now().UTC()
}

func (a PersistentAuthenticator) Authenticate(ctx context.Context, request wire.AuthRequest) (wire.AuthResult, error) {
	if ctx == nil || a.Store == nil || a.Verifier == nil {
		return wire.AuthResult{}, wire.CodeError(wire.ErrAuthenticationFailed, wire.ReasonAuthentication, false, errors.New("persistent authenticator is incomplete"))
	}
	now := a.now()
	if err := request.Validate(now); err != nil {
		return wire.AuthResult{}, wire.CodeError(wire.ErrAuthenticationFailed, wire.ReasonAuthentication, false, err)
	}
	payload, err := wire.AuthProofPayload(request)
	if err != nil {
		return wire.AuthResult{}, wire.CodeError(wire.ErrAuthenticationFailed, wire.ReasonAuthentication, false, err)
	}
	signature, err := wire.DecodeProof(request.SignedProof)
	if err != nil {
		return wire.AuthResult{}, wire.CodeError(wire.ErrAuthenticationFailed, wire.ReasonAuthentication, false, err)
	}
	if err := a.Verifier.VerifyAuthProof(ctx, request, payload, signature); err != nil {
		return wire.AuthResult{}, wire.CodeError(wire.ErrAuthenticationFailed, wire.ReasonAuthentication, false, err)
	}
	// The store performs the durable nonce reservation atomically with its
	// connector/credential authorization. It must reject a replay even after a
	// process restart or on another server replica.
	result, err := a.Store.AuthenticateConnector(ctx, request)
	if err != nil {
		return wire.AuthResult{}, wire.CodeError(wire.ErrAuthenticationFailed, wire.ReasonAuthentication, false, err)
	}
	if err := result.Validate(now); err != nil || !sameAuthIdentity(request, result) {
		if err == nil {
			err = wire.ErrIdentityMismatch
		}
		return wire.AuthResult{}, wire.CodeError(wire.ErrAuthenticationFailed, wire.ReasonAuthentication, false, errors.Join(err, errors.New("persistent credential store returned invalid identity data")))
	}
	return result, nil
}

func (a PersistentAuthenticator) Renew(ctx context.Context, request wire.RenewalRequest) (wire.AuthResult, error) {
	if ctx == nil || a.Store == nil || a.Verifier == nil {
		return wire.AuthResult{}, wire.CodeError(wire.ErrCredentialExpired, wire.ReasonCredentialExpired, true, errors.New("persistent authenticator is incomplete"))
	}
	now := a.now()
	// Freshness is part of the server-side acceptance rule. Validate() alone is
	// insufficient because a signed renewal can otherwise be replayed after a
	// process restart when the in-memory nonce map is gone.
	if err := request.ValidateAt(now); err != nil {
		return wire.AuthResult{}, wire.CodeError(wire.ErrAuthenticationFailed, wire.ReasonAuthentication, false, err)
	}
	payload, err := wire.RenewalProofPayload(request)
	if err != nil {
		return wire.AuthResult{}, wire.CodeError(wire.ErrAuthenticationFailed, wire.ReasonAuthentication, false, err)
	}
	signature, err := wire.DecodeProof(request.SignedProof)
	if err != nil {
		return wire.AuthResult{}, wire.CodeError(wire.ErrAuthenticationFailed, wire.ReasonAuthentication, false, err)
	}
	if err := a.Verifier.VerifyRenewalProof(ctx, request, payload, signature); err != nil {
		return wire.AuthResult{}, wire.CodeError(wire.ErrAuthenticationFailed, wire.ReasonAuthentication, false, err)
	}
	result, err := a.Store.RenewConnector(ctx, request)
	if err != nil {
		return wire.AuthResult{}, wire.CodeError(wire.ErrCredentialExpired, wire.ReasonCredentialExpired, true, err)
	}
	if err := result.Validate(now); err != nil || result.AccountID != request.AccountID || result.TunnelID != request.TunnelID || result.ConnectorID != request.ConnectorID || result.HostID != request.HostID || result.IdentityKeyID != request.IdentityKeyID || result.IdentityKeyThumbprint != request.IdentityKeyThumbprint || result.ProcessGeneration != request.ProcessGeneration || result.CredentialGeneration < request.CredentialGeneration {
		if err == nil {
			err = wire.ErrIdentityMismatch
		}
		return wire.AuthResult{}, wire.CodeError(wire.ErrAuthenticationFailed, wire.ReasonAuthentication, false, errors.Join(err, errors.New("persistent credential store returned invalid renewal data")))
	}
	return result, nil
}

func sameAuthIdentity(request wire.AuthRequest, result wire.AuthResult) bool {
	return request.AccountID == result.AccountID && request.TunnelID == result.TunnelID && request.ConnectorID == result.ConnectorID && request.HostID == result.HostID && request.IdentityKeyID == result.IdentityKeyID && request.IdentityKeyThumbprint == result.IdentityKeyThumbprint && request.ProcessGeneration == result.ProcessGeneration && result.CredentialGeneration >= request.CredentialGeneration
}

type PersistentSnapshotSource struct {
	Store PersistentControlStore
}

func (s PersistentSnapshotSource) Snapshot(ctx context.Context, tunnelID string) (wire.Snapshot, error) {
	if ctx == nil || s.Store == nil {
		return wire.Snapshot{}, wire.CodeError(wire.ErrSnapshotRejected, wire.ReasonSnapshotRejected, true, errors.New("persistent snapshot store is not configured"))
	}
	snapshot, err := s.Store.Snapshot(ctx, tunnelID)
	if err != nil {
		return wire.Snapshot{}, err
	}
	if err := snapshot.Validate(); err != nil || snapshot.TunnelID != tunnelID {
		if err == nil {
			err = wire.ErrIdentityMismatch
		}
		return wire.Snapshot{}, wire.CodeError(wire.ErrSnapshotRejected, wire.ReasonSnapshotRejected, false, errors.Join(err, errors.New("persistent snapshot store returned invalid data")))
	}
	return snapshot, nil
}

type PersistentServer struct {
	server      *wire.Server
	store       PersistentControlStore
	telemetryMu sync.RWMutex
	telemetry   *ConnectorTelemetry
}

func NewPersistentServer(store PersistentControlStore, verifier IdentityProofVerifier, config wire.ServerConfig) (*PersistentServer, error) {
	if store == nil || verifier == nil {
		return nil, wire.ErrInvalidInput
	}
	if _, ok := store.(PersistentDrainStore); !ok {
		return nil, wire.ErrInvalidInput
	}
	if _, ok := store.(PersistentSessionMetadataStore); !ok {
		return nil, wire.ErrInvalidInput
	}
	if config.Clock == nil {
		config.Clock = wire.RealClock{}
	}
	if config.Registry == nil {
		config.Registry = wire.NewSessionRegistry()
	}
	config.Authenticator = PersistentAuthenticator{Store: store, Verifier: verifier, Clock: config.Clock}
	config.Snapshots = PersistentSnapshotSource{Store: store}
	server, err := wire.NewServer(config)
	if err != nil {
		return nil, err
	}
	return &PersistentServer{server: server, store: store}, nil
}

// NewRotationCoordinator creates the production rotation adapter only when
// the store exposes every durable boundary required by connector-v1. In
// particular, the old-key verifier is resolved by connector/account/host and
// generation inside SQL, and restart recovery is loaded from the immutable
// target rows. There is intentionally no in-memory-only fallback here.
func (s *PersistentServer) NewRotationCoordinator(plan wire.RotationPlan) (*wire.RotationCoordinator, error) {
	if s == nil || s.server == nil || s.store == nil || plan.Validate() != nil {
		return nil, wire.ErrInvalidInput
	}
	store, ok := s.store.(wire.RotationPersistence)
	if !ok {
		return nil, wire.CodeError(wire.ErrUnsupportedMessage, wire.ReasonCredentialRotation, false, errors.New("rotation persistence is not configured"))
	}
	if _, ok := s.store.(wire.RotationRecoveryStore); !ok {
		return nil, wire.CodeError(wire.ErrUnsupportedMessage, wire.ReasonCredentialRotation, false, errors.New("rotation recovery is not configured"))
	}
	if _, ok := s.store.(wire.RotationSessionAuthorizer); !ok {
		return nil, wire.CodeError(wire.ErrUnsupportedMessage, wire.ReasonCredentialRotation, false, errors.New("rotation session authorization is not configured"))
	}
	verifier, ok := s.store.(interface {
		VerifyRotationOldProof(context.Context, wire.CredentialRotationProof, []byte, []byte) error
	})
	if !ok {
		return nil, wire.CodeError(wire.ErrUnsupportedMessage, wire.ReasonCredentialRotation, false, errors.New("rotation proof verifier is not configured"))
	}
	return wire.NewRotationCoordinator(plan, wire.RotationConfig{
		Store: store,
		Clock: s.server.Clock(),
		VerifyOldProof: func(ctx context.Context, proof wire.CredentialRotationProof, payload, signature []byte) error {
			return verifier.VerifyRotationOldProof(ctx, proof, payload, signature)
		},
	})
}

// ResumeRotation reconstructs one durable aggregate without issuing a new
// challenge. Workers use this after loading the immutable plan from their
// operation queue; all target phases and credential generations are checked by
// the coordinator and SQL recovery store.
func (s *PersistentServer) ResumeRotation(ctx context.Context, plan wire.RotationPlan) (*wire.RotationCoordinator, error) {
	coordinator, err := s.NewRotationCoordinator(plan)
	if err != nil {
		return nil, err
	}
	if err := coordinator.Resume(ctx); err != nil {
		return nil, err
	}
	return coordinator, nil
}

func (s *PersistentServer) ListRotationPlans(ctx context.Context, limit int) ([]wire.RotationPlan, error) {
	if s == nil || s.store == nil || ctx == nil {
		return nil, wire.ErrInvalidInput
	}
	source, ok := s.store.(RotationPlanSource)
	if !ok {
		return nil, wire.CodeError(wire.ErrUnsupportedMessage, wire.ReasonCredentialRotation, false, errors.New("rotation plan source is not configured"))
	}
	return source.ListCredentialRotationPlans(ctx, limit)
}

func (s *PersistentServer) Accept(ctx context.Context, hello wire.Hello) (*PersistentSession, wire.Welcome, wire.Snapshot, error) {
	if s == nil || s.server == nil || s.store == nil || ctx == nil {
		return nil, wire.Welcome{}, wire.Snapshot{}, wire.ErrInvalidInput
	}
	startedAt := time.Now()
	wasReconnect := false
	_, wasReconnect = s.server.ActiveConnector(hello.ConnectorID)
	observer := s.connectorTelemetry()
	attempt := connectorTelemetryIdentity{
		AccountID:            hello.AccountID,
		Reference:            wire.SessionRef{TunnelID: hello.TunnelID, ConnectorID: hello.ConnectorID, ProcessGeneration: hello.ProcessGeneration},
		CredentialGeneration: hello.Auth.CredentialGeneration,
	}
	recordAcceptFailure := func(ref wire.SessionRef) {
		if observer == nil {
			return
		}
		if ref.TunnelID == "" {
			ref = attempt.Reference
		}
		failure := connectorTelemetryIdentity{AccountID: hello.AccountID, Reference: ref, CredentialGeneration: hello.Auth.CredentialGeneration}
		observer.handshake(failure, hello.HostID, servertelemetry.ConnectorHandshakeFailed, time.Since(startedAt))
		observer.connection(failure, hello.HostID, servertelemetry.ConnectorConnectionFailed)
		if wasReconnect {
			observer.reconnect(failure, hello.HostID, servertelemetry.ConnectorReconnectFailed)
		}
	}
	session, welcome, snapshot, err := s.server.Accept(ctx, hello)
	if err != nil {
		recordAcceptFailure(attempt.Reference)
		return nil, wire.Welcome{}, wire.Snapshot{}, err
	}
	metadataStore := s.store.(PersistentSessionMetadataStore)
	persistErr := metadataStore.CreateConnectorSessionV1(ctx, session.Reference(), welcome, hello.Auth.CredentialGeneration)
	if persistErr != nil {
		recordAcceptFailure(session.Reference())
		closeErr := session.Close(wire.ReasonProtocolClosed)
		(&PersistentSession{session: session, store: s.store, parent: s}).recordClosedTelemetry()
		return nil, wire.Welcome{}, wire.Snapshot{}, fmt.Errorf("create connector session: %w", errors.Join(persistErr, closeErr))
	}
	if observer != nil {
		accepted := connectorTelemetryIdentity{AccountID: hello.AccountID, Reference: session.Reference(), CredentialGeneration: hello.Auth.CredentialGeneration, ConfigGeneration: snapshot.Generation}
		observer.handshake(accepted, hello.HostID, servertelemetry.ConnectorHandshakeSucceeded, time.Since(startedAt))
		observer.connection(accepted, hello.HostID, servertelemetry.ConnectorConnectionOpened)
		observer.config(accepted, hello.HostID, servertelemetry.ConfigGenerationDesired, snapshot.Generation, servertelemetry.RetryNone, time.Time{})
		if wasReconnect {
			observer.reconnect(accepted, hello.HostID, servertelemetry.ConnectorReconnectSucceeded)
		}
	}
	return &PersistentSession{session: session, store: s.store, parent: s}, welcome, snapshot, nil
}

type PersistentSession struct {
	session            *wire.ServerSession
	store              PersistentControlStore
	parent             *PersistentServer
	telemetryCloseOnce sync.Once
}

func (s *PersistentSession) Reference() wire.SessionRef {
	if s == nil || s.session == nil {
		return wire.SessionRef{}
	}
	return s.session.Reference()
}

// failClosed withdraws readiness whenever durable state cannot confirm the
// in-memory transition. Returning a typed conflict while closing is safer than
// leaving a process eligible with a lease/config state the database did not
// record.
func (s *PersistentSession) failClosed(ctx context.Context, primary error) error {
	if s == nil || s.session == nil || s.store == nil {
		return primary
	}
	closeErr := s.session.Close(wire.ReasonProtocolClosed)
	// Durable cleanup must not inherit a canceled request context. Closing the
	// in-memory session is immediate; the database record gets its own short,
	// bounded context so a failed write cannot leave a stale live row forever.
	disconnectErr := s.recordDisconnectedBounded()
	s.recordClosedTelemetry()
	return wire.CodeError(wire.ErrSessionConflict, wire.ReasonProtocolClosed, false, errors.Join(primary, closeErr, disconnectErr))
}

func (s *PersistentSession) recordDisconnectedBounded() error {
	if s == nil || s.session == nil || s.store == nil {
		return nil
	}
	cleanupCtx, cancel := context.WithTimeout(context.Background(), wire.DefaultAbortTimeout)
	defer cancel()
	err := s.store.RecordDisconnected(cleanupCtx, s.session.Reference(), s.session.Disconnect())
	s.recordClosedTelemetry()
	return err
}

func (s *PersistentSession) HandleAck(ctx context.Context, ack wire.Ack) error {
	if s == nil || s.session == nil || s.store == nil || ctx == nil {
		return wire.ErrInvalidInput
	}
	persistNegative := s.session.CanPersistNegativeAck(ack)
	if err := s.session.HandleAck(ctx, ack); err != nil {
		if persistNegative {
			if persistErr := s.store.RecordApplied(ctx, s.session.Reference(), ack); persistErr != nil {
				s.recordClosedTelemetry()
				return s.failClosed(ctx, errors.Join(err, persistErr))
			}
		}
		s.recordClosedTelemetry()
		return err
	}
	if err := s.store.RecordApplied(ctx, s.session.Reference(), ack); err != nil {
		s.recordClosedTelemetry()
		return s.failClosed(ctx, err)
	}
	if ack.Status == wire.AckApplied {
		s.recordConfigTelemetry(ack, servertelemetry.RetryNone, time.Time{})
	} else if ack.Status == wire.AckRejected {
		s.recordDesiredTelemetry(s.currentSnapshotForTelemetry(ack.Generation), servertelemetry.RetryScheduled, time.Now().UTC().Add(time.Minute))
	} else if ack.Status == wire.AckSnapshotRequired {
		s.recordDesiredTelemetry(s.currentSnapshotForTelemetry(ack.Generation), servertelemetry.RetryWaitForChange, time.Time{})
	}
	return nil
}

func (s *PersistentSession) OfferSnapshot(ctx context.Context, snapshot wire.Snapshot) error {
	if s == nil || s.session == nil || ctx == nil {
		return wire.ErrInvalidInput
	}
	if err := s.session.OfferSnapshot(ctx, snapshot); err != nil {
		s.recordClosedTelemetry()
		return err
	}
	s.recordDesiredTelemetry(snapshot, servertelemetry.RetryNone, time.Time{})
	return nil
}

func (s *PersistentSession) OfferDelta(ctx context.Context, delta wire.Delta) error {
	if s == nil || s.session == nil || ctx == nil {
		return wire.ErrInvalidInput
	}
	if err := s.session.OfferDelta(ctx, delta); err != nil {
		s.recordClosedTelemetry()
		return err
	}
	snapshot := wire.Snapshot{TunnelID: delta.TunnelID, Generation: delta.Generation, ContentHash: delta.ContentHash}
	s.recordDesiredTelemetry(snapshot, servertelemetry.RetryNone, time.Time{})
	return nil
}

func (s *PersistentSession) HandleReadiness(ctx context.Context, readiness wire.Readiness) (wire.Ack, error) {
	if s == nil || s.session == nil || s.store == nil || ctx == nil {
		return wire.Ack{}, wire.ErrInvalidInput
	}
	ack, err := s.session.HandleReadiness(ctx, readiness)
	if err != nil {
		s.recordClosedTelemetry()
		return wire.Ack{}, err
	}
	if err := s.store.RecordReady(ctx, s.session.Reference(), readiness); err != nil {
		s.recordClosedTelemetry()
		return wire.Ack{}, s.failClosed(ctx, err)
	}
	s.recordReadyTelemetry(readiness)
	return ack, nil
}

func (s *PersistentSession) HandleHeartbeat(ctx context.Context, heartbeat wire.Heartbeat) (wire.HeartbeatAck, error) {
	if s == nil || s.session == nil || s.store == nil || ctx == nil {
		return wire.HeartbeatAck{}, wire.ErrInvalidInput
	}
	ack, err := s.session.HandleHeartbeat(ctx, heartbeat)
	if err != nil {
		s.recordClosedTelemetry()
		return wire.HeartbeatAck{}, err
	}
	if err := s.store.RecordHeartbeat(ctx, s.session.Reference(), heartbeat, ack); err != nil {
		s.recordClosedTelemetry()
		return wire.HeartbeatAck{}, s.failClosed(ctx, err)
	}
	return ack, nil
}

func (s *PersistentSession) Renew(ctx context.Context, request wire.RenewalRequest) (wire.AuthResult, error) {
	if s == nil || s.session == nil || s.store == nil || ctx == nil {
		return wire.AuthResult{}, wire.ErrInvalidInput
	}
	result, err := s.session.Renew(ctx, request)
	if err != nil {
		s.recordClosedTelemetry()
		return wire.AuthResult{}, err
	}
	if err := s.store.RecordRenewal(ctx, s.session.Reference(), result); err != nil {
		s.recordClosedTelemetry()
		return wire.AuthResult{}, s.failClosed(ctx, err)
	}
	return result, nil
}

func (s *PersistentSession) BeginDrain(ctx context.Context, drainID string, deadline time.Time, forceAfterDeadline bool) (wire.Drain, error) {
	if s == nil || s.session == nil || s.store == nil || ctx == nil {
		return wire.Drain{}, wire.ErrInvalidInput
	}
	drain, err := s.session.BeginDrain(ctx, drainID, deadline, forceAfterDeadline)
	if err != nil {
		s.recordClosedTelemetry()
		return wire.Drain{}, err
	}
	store := s.store.(PersistentDrainStore)
	if err := store.RecordDrain(ctx, s.session.Reference(), drain, wire.DrainAccepted, 0, ""); err != nil {
		s.recordClosedTelemetry()
		return wire.Drain{}, s.failClosed(ctx, err)
	}
	accountID, hostID, ref, credentialGeneration, configGeneration := s.session.Metadata()
	if observer := s.connectorTelemetry(); observer != nil {
		observer.session(connectorTelemetryIdentity{AccountID: accountID, Reference: ref, CredentialGeneration: credentialGeneration, ConfigGeneration: configGeneration}, hostID, servertelemetry.ConnectorSessionDraining, configGeneration)
	}
	return drain, nil
}

func (s *PersistentSession) HandleDrainAck(ctx context.Context, ack wire.DrainAck) error {
	if s == nil || s.session == nil || s.store == nil || ctx == nil {
		return wire.ErrInvalidInput
	}
	persistNegative := s.session.CanPersistNegativeDrainAck(ack)
	if err := s.session.HandleDrainAck(ctx, ack); err != nil {
		if persistNegative {
			request, _, _ := s.session.Drain()
			if persistErr := s.store.(PersistentDrainStore).RecordDrain(ctx, s.session.Reference(), request, ack.Status, ack.ActiveStreams, ack.Code); persistErr != nil {
				s.recordClosedTelemetry()
				return s.failClosed(ctx, errors.Join(err, persistErr))
			}
		}
		s.recordClosedTelemetry()
		return err
	}
	store := s.store.(PersistentDrainStore)
	request, _, _ := s.session.Drain()
	if err := store.RecordDrain(ctx, s.session.Reference(), request, ack.Status, ack.ActiveStreams, ack.Code); err != nil {
		s.recordClosedTelemetry()
		return s.failClosed(ctx, err)
	}
	return nil
}

func (s *PersistentSession) Close(ctx context.Context, reason wire.DisconnectReason) error {
	if s == nil || s.session == nil || s.store == nil || ctx == nil {
		return wire.ErrInvalidInput
	}
	if err := s.session.Close(reason); err != nil {
		s.recordClosedTelemetry()
		return err
	}
	// Use a detached bounded cleanup context. A canceled RPC or shutdown
	// context must not strand the durable session as live.
	if err := s.recordDisconnectedBounded(); err != nil {
		s.recordClosedTelemetry()
		return wire.CodeError(wire.ErrSessionConflict, wire.ReasonProtocolClosed, false, err)
	}
	s.recordClosedTelemetry()
	return nil
}

func (s *PersistentSession) CheckLease(ctx context.Context, now time.Time) error {
	if s == nil || s.session == nil || s.store == nil || ctx == nil {
		return wire.ErrInvalidInput
	}
	err := s.session.CheckLease(now)
	if err == nil {
		return nil
	}
	if s.session.State() == wire.SessionClosed {
		s.recordClosedTelemetry()
		if recordErr := s.recordDisconnectedBounded(); recordErr != nil {
			return errors.Join(err, recordErr)
		}
	}
	return err
}

func (s *PersistentSession) Current() (wire.Snapshot, bool, uint64) {
	if s == nil || s.session == nil {
		return wire.Snapshot{}, false, 0
	}
	return s.session.Current()
}

func (s *PersistentSession) Candidate() (wire.Snapshot, bool) {
	if s == nil || s.session == nil {
		return wire.Snapshot{}, false
	}
	return s.session.Candidate()
}
