package connectorprotocol

import (
	"context"
	"crypto/sha256"
	"errors"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/workers"
	wire "github.com/pinksaucepasta/paperboat/shared/connectorprotocol"
)

const (
	defaultRotationReconcileInterval = 5 * time.Second
	defaultRotationChallengeTTL      = 5 * time.Minute
	defaultRotationOverlap           = 30 * time.Minute
	defaultRotationCredentialLife    = 365 * 24 * time.Hour
	defaultRotationPlanLimit         = 64
	defaultRotationSendTimeout       = 5 * time.Second
)

// RotationFrameSender is the only transport dependency of RotationDispatcher.
// TRK-14 binds it to the multiplexed control carrier. The callback must return
// only after the frame has been accepted by that carrier's bounded send queue.
type RotationFrameSender func(context.Context, wire.Frame) error

type RotationOperationPlanSource interface {
	LoadCredentialRotationPlan(context.Context, string) (wire.RotationPlan, error)
}

type RotationTargetRecoveryStore interface {
	ResetCredentialRotationInstall(context.Context, wire.RotationPlan, wire.RotationTarget, wire.SessionRef) error
	RebindCredentialRotationRevoke(context.Context, wire.RotationPlan, wire.RotationTarget, wire.SessionRef, string, time.Time, time.Time) (wire.CredentialRotationRevoke, error)
	FailCredentialRotationTarget(context.Context, wire.RotationPlan, wire.RotationTarget, wire.Code) error
}

// RotationDispatcherStore is one durable source of truth for transitions and
// restart recovery. Keeping these methods on one object prevents a dispatcher
// from reading plans from a different database than the one it mutates.
type RotationDispatcherStore interface {
	wire.RotationPersistence
	RotationPlanSource
	wire.RotationRecoveryStore
	RotationOperationPlanSource
	RotationTargetRecoveryStore
	RecordDisconnected(context.Context, wire.SessionRef, wire.Disconnect) error
}

// RotationLiveSession is the authenticated, negotiated session metadata needed
// to safely deliver one connector's rotation messages. It deliberately contains
// no private key or bearer credential bytes.
type RotationLiveSession struct {
	AccountID              string
	HostID                 string
	Reference              wire.SessionRef
	CredentialGeneration   uint64
	IdentityKeyID          string
	IdentityKeyThumbprint  string
	NegotiatedCapabilities []string
	Send                   RotationFrameSender
}

func (s RotationLiveSession) validate() error {
	if wire.ValidateIdentifier(s.AccountID) != nil || wire.ValidateIdentifier(s.HostID) != nil || s.Reference.Validate() != nil || s.CredentialGeneration == 0 || wire.ValidateIdentityKey(s.IdentityKeyID, s.IdentityKeyThumbprint) != nil || s.Send == nil {
		return wire.ErrInvalidInput
	}
	if !wire.HasCapability(s.NegotiatedCapabilities, wire.CapabilityCredentialRotation) {
		return wire.CodeError(wire.ErrCapabilityMissing, wire.ReasonCapabilityMissing, false, nil)
	}
	return nil
}

// RotationDispatcherConfig controls bounded reconciliation and credential
// policy. Store must be durable in production; SQLControlStore implements all
// required interfaces.
type RotationDispatcherConfig struct {
	Store              RotationDispatcherStore
	VerifyOldProof     wire.RotationOldProofVerifier
	Clock              wire.Clock
	ReconcileInterval  time.Duration
	ChallengeTTL       time.Duration
	Overlap            time.Duration
	CredentialLifetime time.Duration
	PlanLimit          int
	SendTimeout        time.Duration
	ReportError        func(error)
}

type rotationDelivery struct {
	reference wire.SessionRef
	payload   [sha256.Size]byte
}

// RotationDispatcher joins durable aggregate rotation state to exact live
// connector sessions. RegisterSession/UnregisterSession are called by the
// control carrier. ReconcileOnce is safe to retry after process or network
// failure because every phase is persisted before its frame is emitted.
type RotationDispatcher struct {
	store              RotationDispatcherStore
	verify             wire.RotationOldProofVerifier
	clock              wire.Clock
	reconcileInterval  time.Duration
	challengeTTL       time.Duration
	overlap            time.Duration
	credentialLifetime time.Duration
	planLimit          int
	sendTimeout        time.Duration
	reportError        func(error)

	mu         sync.RWMutex
	sessions   map[string]RotationLiveSession
	deliveries map[string]rotationDelivery
}

func NewRotationDispatcher(config RotationDispatcherConfig) (*RotationDispatcher, error) {
	if config.Store == nil || config.VerifyOldProof == nil || config.ReportError == nil {
		return nil, wire.ErrInvalidInput
	}
	clock := config.Clock
	if clock == nil {
		clock = wire.RealClock{}
	}
	reconcileInterval := config.ReconcileInterval
	if reconcileInterval == 0 {
		reconcileInterval = defaultRotationReconcileInterval
	}
	challengeTTL := config.ChallengeTTL
	if challengeTTL == 0 {
		challengeTTL = defaultRotationChallengeTTL
	}
	overlap := config.Overlap
	if overlap == 0 {
		overlap = defaultRotationOverlap
	}
	credentialLifetime := config.CredentialLifetime
	if credentialLifetime == 0 {
		credentialLifetime = defaultRotationCredentialLife
	}
	planLimit := config.PlanLimit
	if planLimit == 0 {
		planLimit = defaultRotationPlanLimit
	}
	sendTimeout := config.SendTimeout
	if sendTimeout == 0 {
		sendTimeout = defaultRotationSendTimeout
	}
	if reconcileInterval <= 0 || challengeTTL <= 0 || challengeTTL > 15*time.Minute || overlap <= challengeTTL || overlap > wire.MaxLease || credentialLifetime <= overlap || credentialLifetime > wire.MaxRotationCredentialLifetime || planLimit < 1 || planLimit > 64 || sendTimeout <= 0 || sendTimeout > time.Minute {
		return nil, wire.ErrInvalidInput
	}
	return &RotationDispatcher{
		store: config.Store, verify: config.VerifyOldProof, clock: clock,
		reconcileInterval: reconcileInterval, challengeTTL: challengeTTL, overlap: overlap, credentialLifetime: credentialLifetime, planLimit: planLimit, sendTimeout: sendTimeout, reportError: config.ReportError,
		sessions: make(map[string]RotationLiveSession), deliveries: make(map[string]rotationDelivery),
	}, nil
}

// RegisterSession atomically replaces only an older process generation. An
// equal or stale generation cannot steal delivery from the active carrier.
func (d *RotationDispatcher) RegisterSession(ctx context.Context, session RotationLiveSession) error {
	if d == nil || ctx == nil {
		return wire.ErrInvalidInput
	}
	if err := session.validate(); err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	current, ok := d.sessions[session.Reference.ConnectorID]
	if ok {
		if current.AccountID != session.AccountID || current.HostID != session.HostID || current.Reference.TunnelID != session.Reference.TunnelID {
			return wire.CodeError(wire.ErrIdentityMismatch, wire.ReasonAuthentication, false, nil)
		}
		if session.Reference.ProcessGeneration <= current.Reference.ProcessGeneration {
			return wire.CodeError(wire.ErrSessionConflict, wire.ReasonStaleGeneration, false, nil)
		}
		disconnect := wire.Disconnect{AccountID: current.AccountID, TunnelID: current.Reference.TunnelID, ConnectorID: current.Reference.ConnectorID, SessionID: current.Reference.SessionID, ProcessGeneration: current.Reference.ProcessGeneration, Reason: wire.ReasonSessionReplaced, Retryable: true}
		if err := d.store.RecordDisconnected(ctx, current.Reference, disconnect); err != nil {
			return err
		}
	}
	session.NegotiatedCapabilities = append([]string(nil), session.NegotiatedCapabilities...)
	d.sessions[session.Reference.ConnectorID] = session
	return nil
}

// UnregisterSession is generation-safe: a late close from an old carrier does
// not remove its replacement.
func (d *RotationDispatcher) UnregisterSession(ctx context.Context, ref wire.SessionRef, reason wire.DisconnectReason) error {
	if d == nil || ctx == nil || ref.Validate() != nil || !wire.ValidDisconnectReason(reason) {
		return wire.ErrInvalidInput
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	current, ok := d.sessions[ref.ConnectorID]
	if !ok {
		return nil
	}
	if current.Reference != ref {
		return wire.CodeError(wire.ErrStaleSession, wire.ReasonSessionReplaced, false, nil)
	}
	disconnect := wire.Disconnect{AccountID: current.AccountID, TunnelID: ref.TunnelID, ConnectorID: ref.ConnectorID, SessionID: ref.SessionID, ProcessGeneration: ref.ProcessGeneration, Reason: reason, Retryable: reason != wire.ReasonAuthentication && reason != wire.ReasonCredentialExpired}
	if err := d.store.RecordDisconnected(ctx, ref, disconnect); err != nil {
		return err
	}
	delete(d.sessions, ref.ConnectorID)
	return nil
}

// DetachSession removes only the in-memory delivery route. The control
// transport uses it after PersistentSession.Close has already recorded the
// durable disconnect, avoiding a duplicate persistence transition.
func (d *RotationDispatcher) DetachSession(ref wire.SessionRef) {
	if d == nil || ref.Validate() != nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	current, ok := d.sessions[ref.ConnectorID]
	if !ok || current.Reference != ref {
		return
	}
	delete(d.sessions, ref.ConnectorID)
	for key, delivery := range d.deliveries {
		if delivery.reference == ref {
			delete(d.deliveries, key)
		}
	}
}

func (d *RotationDispatcher) session(connectorID string) (RotationLiveSession, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	session, ok := d.sessions[connectorID]
	return session, ok
}

func (d *RotationDispatcher) coordinator(ctx context.Context, plan wire.RotationPlan) (*wire.RotationCoordinator, wire.RotationResume, error) {
	coordinator, err := wire.NewRotationCoordinator(plan, wire.RotationConfig{Store: d.store, VerifyOldProof: d.verify, Clock: d.clock})
	if err != nil {
		return nil, wire.RotationResume{}, err
	}
	if err := coordinator.Resume(ctx); err != nil {
		return nil, wire.RotationResume{}, err
	}
	resume, err := d.store.LoadCredentialRotation(ctx, plan)
	return coordinator, resume, err
}

func (d *RotationDispatcher) findCoordinator(ctx context.Context, operationID string) (*wire.RotationCoordinator, wire.RotationPlan, error) {
	if wire.ValidateIdentifier(operationID) != nil {
		return nil, wire.RotationPlan{}, wire.ErrInvalidInput
	}
	plan, err := d.store.LoadCredentialRotationPlan(ctx, operationID)
	if err != nil {
		return nil, wire.RotationPlan{}, err
	}
	coordinator, _, err := d.coordinator(ctx, plan)
	return coordinator, plan, err
}

func (d *RotationDispatcher) send(ctx context.Context, session RotationLiveSession, messageType wire.MessageType, payload any) error {
	requestID, err := wire.NewOpaqueID("request")
	if err != nil {
		return err
	}
	frame, err := wire.NewFrame(messageType, requestID, payload)
	if err != nil {
		return err
	}
	key, err := rotationDeliveryKey(messageType, payload)
	if err != nil {
		return err
	}
	digest := sha256.Sum256(frame.Payload)
	d.mu.RLock()
	current, ok := d.sessions[session.Reference.ConnectorID]
	if !ok || current.Reference != session.Reference || current.AccountID != session.AccountID || current.HostID != session.HostID || current.CredentialGeneration != session.CredentialGeneration || current.IdentityKeyID != session.IdentityKeyID || current.IdentityKeyThumbprint != session.IdentityKeyThumbprint {
		d.mu.RUnlock()
		return wire.CodeError(wire.ErrStaleSession, wire.ReasonSessionReplaced, false, nil)
	}
	if delivered, ok := d.deliveries[key]; ok && delivered.reference == current.Reference && delivered.payload == digest {
		d.mu.RUnlock()
		return nil
	}
	sendCtx, cancel := context.WithTimeout(ctx, d.sendTimeout)
	err = current.Send(sendCtx, frame)
	cancel()
	d.mu.RUnlock()
	if err != nil {
		return err
	}
	d.mu.Lock()
	if latest, ok := d.sessions[current.Reference.ConnectorID]; ok && latest.Reference == current.Reference {
		d.deliveries[key] = rotationDelivery{reference: current.Reference, payload: digest}
	}
	d.mu.Unlock()
	return nil
}

func rotationDeliveryKey(messageType wire.MessageType, payload any) (string, error) {
	var operationID, connectorID string
	switch value := payload.(type) {
	case wire.CredentialRotationChallenge:
		operationID, connectorID = value.OperationID, value.ConnectorID
	case wire.CredentialRotationInstall:
		operationID, connectorID = value.OperationID, value.ConnectorID
	case wire.CredentialRotationRevoke:
		operationID, connectorID = value.OperationID, value.ConnectorID
	default:
		return "", wire.ErrInvalidInput
	}
	if wire.ValidateIdentifier(operationID) != nil || wire.ValidateIdentifier(connectorID) != nil {
		return "", wire.ErrInvalidInput
	}
	return operationID + "\x00" + connectorID + "\x00" + string(messageType), nil
}

// ReconcileOnce resumes every bounded running plan and re-emits only the
// durable phase appropriate for each exact target session. A connector sees
// only its own target, never the aggregate membership.
func (d *RotationDispatcher) ReconcileOnce(ctx context.Context) error {
	if d == nil || ctx == nil {
		return wire.ErrInvalidInput
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	plans, err := d.store.ListCredentialRotationPlans(ctx, d.planLimit)
	if err != nil {
		return err
	}
	var joined error
	for _, plan := range plans {
		if err := ctx.Err(); err != nil {
			return errors.Join(joined, err)
		}
		coordinator, resume, loadErr := d.coordinator(ctx, plan)
		if loadErr != nil {
			joined = errors.Join(joined, loadErr)
			continue
		}
		for _, target := range resume.Targets {
			if err := d.reconcileTarget(ctx, plan, coordinator, resume.Targets, target); err != nil {
				joined = errors.Join(joined, err)
			}
		}
	}
	return joined
}

func (d *RotationDispatcher) reconcileTarget(ctx context.Context, plan wire.RotationPlan, coordinator *wire.RotationCoordinator, targets []wire.RotationResumeTarget, recovered wire.RotationResumeTarget) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	now := d.clock.Now().UTC()
	if (recovered.State == wire.RotationTargetPending || recovered.State == wire.RotationTargetChallenged || recovered.State == wire.RotationTargetInstalled) && !recovered.OverlapUntil.IsZero() && !recovered.OverlapUntil.After(now) {
		if err := d.store.FailCredentialRotationTarget(ctx, plan, recovered.Target, wire.CodeCredentialExpired); err != nil {
			return err
		}
		return wire.CodeError(wire.ErrCredentialExpired, wire.ReasonCredentialExpired, false, nil)
	}
	session, online := d.session(recovered.Target.ConnectorID)
	switch recovered.State {
	case wire.RotationTargetPending:
		if !online || session.AccountID != plan.AccountID || session.HostID != recovered.Target.HostID || session.Reference.TunnelID != plan.TunnelID || session.CredentialGeneration != recovered.Target.OldCredentialGeneration {
			return nil
		}
		nonce, err := wire.NewOpaqueID("rotation-nonce")
		if err != nil {
			return err
		}
		expiresAt, overlapUntil, credentialValidUntil, err := d.rotationPolicy(now, recovered)
		if err != nil {
			return err
		}
		challenge, err := coordinator.ChallengeWithCredentialExpiry(ctx, recovered.Target.ConnectorID, session.Reference.SessionID, session.Reference.ProcessGeneration, session.IdentityKeyID, session.IdentityKeyThumbprint, nonce, now, expiresAt, overlapUntil, credentialValidUntil)
		if err != nil {
			return err
		}
		return d.send(ctx, session, wire.MessageCredentialRotationChallenge, challenge)
	case wire.RotationTargetChallenged:
		challenge := recovered.Challenge
		if !online {
			return nil
		}
		if !challenge.ExpiresAt.After(now) {
			if !liveSessionMatchesRotationTarget(session, plan, recovered.Target) {
				return nil
			}
			nonce, err := wire.NewOpaqueID("rotation-nonce")
			if err != nil {
				return err
			}
			expiresAt, overlapUntil, credentialValidUntil, err := d.rotationPolicy(now, recovered)
			if err != nil {
				return err
			}
			fresh, err := coordinator.ChallengeWithCredentialExpiry(ctx, recovered.Target.ConnectorID, session.Reference.SessionID, session.Reference.ProcessGeneration, session.IdentityKeyID, session.IdentityKeyThumbprint, nonce, now, expiresAt, overlapUntil, credentialValidUntil)
			if err != nil {
				return err
			}
			challenge = fresh
		} else if !liveSessionMatchesChallenge(session, challenge) {
			return nil
		}
		return d.send(ctx, session, wire.MessageCredentialRotationChallenge, challenge)
	case wire.RotationTargetInstalled:
		if !online {
			return nil
		}
		if session.Reference.SessionID != recovered.Install.SessionID || session.Reference.ProcessGeneration != recovered.Install.ProcessGeneration || session.CredentialGeneration != recovered.Install.OldCredentialGeneration {
			if liveSessionMatchesRotationTarget(session, plan, recovered.Target) && session.Reference.ProcessGeneration > recovered.Install.ProcessGeneration {
				return d.store.ResetCredentialRotationInstall(ctx, plan, recovered.Target, session.Reference)
			}
			return nil
		}
		return d.send(ctx, session, wire.MessageCredentialRotationInstall, recovered.Install)
	case wire.RotationTargetReady:
		if !allRotationTargetsReady(targets) {
			return nil
		}
		revoke, err := coordinator.Revoke(ctx, recovered.Target.ConnectorID)
		if err != nil {
			return err
		}
		if !online || !liveSessionMatchesRevoke(session, revoke) {
			return nil
		}
		return d.send(ctx, session, wire.MessageCredentialRotationRevoke, revoke)
	case wire.RotationTargetRevoking:
		if !recovered.Revoke.Deadline.After(now) {
			_, err := coordinator.Fail(ctx, recovered.Target.ConnectorID, wire.CodeCredentialRotationFailed)
			return err
		}
		if online && session.AccountID == plan.AccountID && session.HostID == recovered.Target.HostID && session.Reference.TunnelID == plan.TunnelID && session.Reference.ConnectorID == recovered.Target.ConnectorID && session.CredentialGeneration == recovered.Target.NewCredentialGeneration && session.Reference.ProcessGeneration > recovered.Revoke.ProcessGeneration {
			nonce, err := wire.NewOpaqueID("rotation-revoke")
			if err != nil {
				return err
			}
			rebound, err := d.store.RebindCredentialRotationRevoke(ctx, plan, recovered.Target, session.Reference, nonce, now, now.Add(wire.DefaultAbortTimeout))
			if err != nil {
				return err
			}
			return d.send(ctx, session, wire.MessageCredentialRotationRevoke, rebound)
		}
		if online && liveSessionMatchesRevoke(session, recovered.Revoke) {
			return d.send(ctx, session, wire.MessageCredentialRotationRevoke, recovered.Revoke)
		}
	}
	return nil
}

func (d *RotationDispatcher) rotationPolicy(now time.Time, recovered wire.RotationResumeTarget) (time.Time, time.Time, time.Time, error) {
	overlapUntil := recovered.OverlapUntil
	credentialValidUntil := recovered.NewCredentialValidUntil
	if overlapUntil.IsZero() && credentialValidUntil.IsZero() {
		// In-memory protocol tests may begin a plan directly. Production API
		// rows always carry the immutable policy captured with the operation.
		overlapUntil = now.Add(d.overlap)
		credentialValidUntil = now.Add(d.credentialLifetime)
	}
	if !overlapUntil.After(now) || !credentialValidUntil.After(overlapUntil) {
		return time.Time{}, time.Time{}, time.Time{}, wire.CodeError(wire.ErrCredentialExpired, wire.ReasonCredentialExpired, false, nil)
	}
	expiresAt := now.Add(d.challengeTTL)
	if !overlapUntil.After(expiresAt) {
		expiresAt = overlapUntil.Add(-time.Millisecond)
	}
	if !expiresAt.After(now) {
		return time.Time{}, time.Time{}, time.Time{}, wire.CodeError(wire.ErrCredentialExpired, wire.ReasonCredentialExpired, false, nil)
	}
	return expiresAt, overlapUntil, credentialValidUntil, nil
}

func allRotationTargetsReady(targets []wire.RotationResumeTarget) bool {
	if len(targets) == 0 {
		return false
	}
	for _, target := range targets {
		if target.State != wire.RotationTargetReady && target.State != wire.RotationTargetRevoking && target.State != wire.RotationTargetRevoked {
			return false
		}
	}
	return true
}

func liveSessionMatchesChallenge(session RotationLiveSession, challenge wire.CredentialRotationChallenge) bool {
	return session.AccountID == challenge.AccountID && session.HostID == challenge.HostID && session.Reference.TunnelID == challenge.TunnelID && session.Reference.ConnectorID == challenge.ConnectorID && session.Reference.SessionID == challenge.SessionID && session.Reference.ProcessGeneration == challenge.ProcessGeneration && session.CredentialGeneration == challenge.OldCredentialGeneration && session.IdentityKeyID == challenge.OldIdentityKeyID && session.IdentityKeyThumbprint == challenge.OldIdentityKeyThumbprint && wire.HasCapability(session.NegotiatedCapabilities, wire.CapabilityCredentialRotation)
}

func liveSessionMatchesRotationTarget(session RotationLiveSession, plan wire.RotationPlan, target wire.RotationTarget) bool {
	return session.AccountID == plan.AccountID && session.HostID == target.HostID && session.Reference.TunnelID == plan.TunnelID && session.Reference.ConnectorID == target.ConnectorID && session.CredentialGeneration == target.OldCredentialGeneration && wire.HasCapability(session.NegotiatedCapabilities, wire.CapabilityCredentialRotation)
}

func liveSessionMatchesRevoke(session RotationLiveSession, revoke wire.CredentialRotationRevoke) bool {
	return session.AccountID == revoke.AccountID && session.HostID == revoke.HostID && session.Reference.TunnelID == revoke.TunnelID && session.Reference.ConnectorID == revoke.ConnectorID && session.Reference.SessionID == revoke.SessionID && session.Reference.ProcessGeneration == revoke.ProcessGeneration && session.CredentialGeneration == revoke.NewCredentialGeneration && wire.HasCapability(session.NegotiatedCapabilities, wire.CapabilityCredentialRotation)
}

// HandleFrame routes a connector response only after binding it to the current
// registered session. Durable coordinator/store checks provide the second,
// transactional identity and generation guard.
func (d *RotationDispatcher) HandleFrame(ctx context.Context, ref wire.SessionRef, frame wire.Frame) error {
	if d == nil || ctx == nil || ref.Validate() != nil {
		return wire.ErrInvalidInput
	}
	session, ok := d.session(ref.ConnectorID)
	if !ok || session.Reference != ref {
		return wire.CodeError(wire.ErrStaleSession, wire.ReasonSessionReplaced, false, nil)
	}
	switch frame.Type {
	case wire.MessageCredentialRotationProof:
		var proof wire.CredentialRotationProof
		if err := frame.DecodePayload(&proof); err != nil {
			return err
		}
		if !frameMatchesSession(proof.AccountID, proof.TunnelID, proof.ConnectorID, proof.HostID, proof.SessionID, proof.ProcessGeneration, session) || session.CredentialGeneration != proof.OldCredentialGeneration || session.IdentityKeyID != proof.OldIdentityKeyID || session.IdentityKeyThumbprint != proof.OldIdentityKeyThumbprint {
			return wire.CodeError(wire.ErrIdentityMismatch, wire.ReasonAuthentication, false, nil)
		}
		coordinator, _, err := d.findCoordinator(ctx, proof.OperationID)
		if err != nil {
			return err
		}
		install, _, err := coordinator.AcceptProof(ctx, proof)
		if err != nil {
			return err
		}
		return d.send(ctx, session, wire.MessageCredentialRotationInstall, install)
	case wire.MessageCredentialRotationReady:
		var ready wire.CredentialRotationReady
		if err := frame.DecodePayload(&ready); err != nil {
			return err
		}
		if !frameMatchesSession(ready.AccountID, ready.TunnelID, ready.ConnectorID, ready.HostID, ready.SessionID, ready.ProcessGeneration, session) || session.CredentialGeneration != ready.NewCredentialGeneration || session.IdentityKeyID != ready.NewIdentityKeyID || session.IdentityKeyThumbprint != ready.NewIdentityKeyThumbprint {
			return wire.CodeError(wire.ErrIdentityMismatch, wire.ReasonAuthentication, false, nil)
		}
		coordinator, plan, err := d.findCoordinator(ctx, ready.OperationID)
		if err != nil {
			return err
		}
		if _, err := coordinator.MarkReady(ctx, ready); err != nil {
			return err
		}
		return d.reconcilePlan(ctx, plan, coordinator)
	case wire.MessageCredentialRotationAck:
		var ack wire.CredentialRotationAck
		if err := frame.DecodePayload(&ack); err != nil {
			return err
		}
		if !frameMatchesSession(ack.AccountID, ack.TunnelID, ack.ConnectorID, ack.HostID, ack.SessionID, ack.ProcessGeneration, session) {
			return wire.CodeError(wire.ErrIdentityMismatch, wire.ReasonAuthentication, false, nil)
		}
		coordinator, _, err := d.findCoordinator(ctx, ack.OperationID)
		if err != nil {
			return err
		}
		_, err = coordinator.HandleAck(ctx, ack)
		return err
	default:
		return wire.CodeError(wire.ErrUnsupportedMessage, wire.ReasonCredentialRotation, false, nil)
	}
}

func (d *RotationDispatcher) reconcilePlan(ctx context.Context, plan wire.RotationPlan, coordinator *wire.RotationCoordinator) error {
	resume, err := d.store.LoadCredentialRotation(ctx, plan)
	if err != nil || !allRotationTargetsReady(resume.Targets) {
		return err
	}
	var joined error
	for _, target := range resume.Targets {
		if err := ctx.Err(); err != nil {
			return errors.Join(joined, err)
		}
		if target.State != wire.RotationTargetReady {
			continue
		}
		revoke, revokeErr := coordinator.Revoke(ctx, target.Target.ConnectorID)
		if revokeErr != nil {
			joined = errors.Join(joined, revokeErr)
			continue
		}
		session, ok := d.session(target.Target.ConnectorID)
		if !ok || !liveSessionMatchesRevoke(session, revoke) {
			continue
		}
		joined = errors.Join(joined, d.send(ctx, session, wire.MessageCredentialRotationRevoke, revoke))
	}
	return joined
}

func frameMatchesSession(accountID, tunnelID, connectorID, hostID, sessionID string, processGeneration uint64, session RotationLiveSession) bool {
	return accountID == session.AccountID && tunnelID == session.Reference.TunnelID && connectorID == session.Reference.ConnectorID && hostID == session.HostID && sessionID == session.Reference.SessionID && processGeneration == session.Reference.ProcessGeneration && wire.HasCapability(session.NegotiatedCapabilities, wire.CapabilityCredentialRotation)
}

// Run reconciles immediately and then at a bounded interval until canceled.
func (d *RotationDispatcher) Run(ctx context.Context) error {
	if d == nil || ctx == nil {
		return wire.ErrInvalidInput
	}
	attempt := 0
	for {
		finishProgress := workers.BeginIteration(ctx, d.reconcileInterval)
		err := d.ReconcileOnce(ctx)
		if err != nil {
			workers.IterationFailure(ctx, "connector_rotation", "connector_rotation_failed", err)
		}
		finishProgress(err)
		delay := d.reconcileInterval
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			d.reportError(err)
			attempt++
			delay = rotationRetryDelay(d.reconcileInterval, attempt)
		} else {
			attempt = 0
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func rotationRetryDelay(base time.Duration, attempt int) time.Duration {
	capDelay := base
	for i := 1; i < attempt && capDelay < time.Minute; i++ {
		capDelay *= 2
		if capDelay > time.Minute {
			capDelay = time.Minute
		}
	}
	if capDelay <= 1 {
		return capDelay
	}
	return time.Duration(rand.Int64N(int64(capDelay) + 1))
}
