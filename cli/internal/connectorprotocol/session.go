package connectorprotocol

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

type SessionState string

const (
	SessionNew              SessionState = "new"
	SessionAwaitingSnapshot SessionState = "awaiting_snapshot"
	SessionAwaitingReady    SessionState = "awaiting_ready"
	SessionReady            SessionState = "ready"
	SessionDraining         SessionState = "draining"
	SessionClosed           SessionState = "closed"
)

func bindSnapshot(snapshot Snapshot, accountID, connectorID, sessionID string, processGeneration uint64) Snapshot {
	snapshot.AccountID = accountID
	snapshot.ConnectorID = connectorID
	snapshot.SessionID = sessionID
	snapshot.ProcessGeneration = processGeneration
	snapshot.Payload = append([]byte(nil), snapshot.Payload...)
	return snapshot
}

func cloneSnapshot(snapshot Snapshot) Snapshot {
	snapshot.Payload = append([]byte(nil), snapshot.Payload...)
	return snapshot
}

func hasCapability(values []string, capability string) bool {
	for _, value := range values {
		if value == capability {
			return true
		}
	}
	return false
}

type ClientSessionConfig struct {
	Hello        Hello
	Applier      ConfigApplier
	Drainer      Drainer
	Clock        Clock
	ApplyTimeout time.Duration
	AbortTimeout time.Duration
}

// Drainer is the narrow runtime hook needed by the control protocol. It must
// stop admitting new streams before returning, report a bounded active-stream
// count, and force-close only under the supplied cancellation/deadline. The
// control session never owns data-plane streams itself.
type Drainer interface {
	StopNewStreams(context.Context) error
	ActiveStreams(context.Context) (uint32, error)
	ForceClose(context.Context) error
}

type noopDrainer struct{}

func (noopDrainer) StopNewStreams(context.Context) error          { return nil }
func (noopDrainer) ActiveStreams(context.Context) (uint32, error) { return 0, nil }
func (noopDrainer) ForceClose(context.Context) error              { return nil }

type ClientSession struct {
	mu               sync.RWMutex
	applyMu          sync.Mutex
	config           ClientSessionConfig
	welcome          Welcome
	state            SessionState
	lease            Lease
	auth             AuthRequest
	active           Snapshot
	hasActive        bool
	candidate        Snapshot
	hasCandidate     bool
	prepared         PreparedConfig
	needsSnapshot    bool
	readyGeneration  uint64
	lastHeartbeatAck time.Time
	drain            *Drain
	drainStatus      DrainStatus
	drainCode        Code
	drainCompleted   bool
	activeStreams    uint32
	disconnectReason DisconnectReason
}

func NewClientSession(config ClientSessionConfig) (*ClientSession, error) {
	if config.Clock == nil {
		config.Clock = realClock{}
	}
	if config.Applier == nil {
		config.Applier = noopApplier{}
	}
	if config.Drainer == nil {
		config.Drainer = noopDrainer{}
	}
	if config.ApplyTimeout == 0 {
		config.ApplyTimeout = DefaultApplyTimeout
	}
	if config.AbortTimeout == 0 {
		config.AbortTimeout = DefaultAbortTimeout
	}
	if config.ApplyTimeout <= 0 || config.ApplyTimeout > MaxLease || config.AbortTimeout <= 0 || config.AbortTimeout > MaxLease {
		return nil, ErrInvalidInput
	}
	if err := config.Hello.Validate(config.Clock.Now().UTC()); err != nil {
		return nil, err
	}
	return &ClientSession{config: config, auth: config.Hello.Auth, state: SessionNew}, nil
}

func (c *ClientSession) Hello() Hello {
	if c == nil {
		return Hello{}
	}
	return c.config.Hello
}

// Auth returns the currently authenticated credential and expiry metadata.
// Control supervisors use this snapshot to schedule renewal without reaching
// into the session's state machine.
func (c *ClientSession) Auth() AuthRequest {
	if c == nil {
		return AuthRequest{}
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.auth
}

func (c *ClientSession) AcceptWelcome(welcome Welcome) error {
	if c == nil {
		return ErrInvalidInput
	}
	if err := welcome.Validate(c.config.Clock.Now().UTC()); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state != SessionNew {
		return codeError(ErrSessionConflict, ReasonProtocolClosed, false, nil)
	}
	if welcome.Version != ProtocolVersion || welcome.Protocol != ProtocolName {
		return codeError(ErrProtocolIncompatible, ReasonProtocolMismatch, false, nil)
	}
	if _, err := NegotiateVersion(c.config.Hello.MinVersion, c.config.Hello.MaxVersion); err != nil {
		return err
	}
	if _, err := NegotiateCapabilities(c.config.Hello.Capabilities, welcome.Capabilities); err != nil {
		return err
	}
	c.welcome = welcome
	c.lease = welcome.Lease
	c.state = SessionAwaitingSnapshot
	c.lastHeartbeatAck = c.config.Clock.Now().UTC()
	return nil
}

func (c *ClientSession) ensureActiveLocked(now time.Time) error {
	if c.state == SessionClosed {
		return codeError(ErrSessionClosed, c.disconnectReason, false, nil)
	}
	if c.state == SessionNew {
		return codeError(ErrSessionConflict, ReasonProtocolClosed, false, nil)
	}
	if !c.lease.ExpiresAt.After(now) {
		c.state = SessionClosed
		c.disconnectReason = ReasonLeaseExpired
		return codeError(ErrLeaseExpired, ReasonLeaseExpired, true, nil)
	}
	if !c.lastHeartbeatAck.IsZero() && now.Sub(c.lastHeartbeatAck) > 2*time.Duration(c.lease.HeartbeatIntervalMS)*time.Millisecond {
		c.state = SessionClosed
		c.disconnectReason = ReasonHeartbeatTimeout
		return codeError(ErrHeartbeatTimeout, ReasonHeartbeatTimeout, true, nil)
	}
	return nil
}

func (c *ClientSession) ApplySnapshot(ctx context.Context, snapshot Snapshot) (Ack, error) {
	if c == nil || ctx == nil {
		return Ack{}, ErrInvalidInput
	}
	if err := snapshot.Validate(); err != nil {
		return Ack{}, codeError(ErrSnapshotRejected, ReasonSnapshotRejected, false, err)
	}
	c.applyMu.Lock()
	defer c.applyMu.Unlock()
	c.mu.Lock()
	if err := c.ensureActiveLocked(c.config.Clock.Now().UTC()); err != nil {
		c.mu.Unlock()
		return Ack{}, err
	}
	if snapshot.TunnelID != c.config.Hello.TunnelID {
		c.mu.Unlock()
		return Ack{}, codeError(ErrIdentityMismatch, ReasonAuthentication, false, nil)
	}
	if err := snapshot.ValidateBound(); err != nil || snapshot.AccountID != c.config.Hello.AccountID || snapshot.ConnectorID != c.config.Hello.ConnectorID || snapshot.SessionID != c.welcome.SessionID || snapshot.ProcessGeneration != c.config.Hello.ProcessGeneration {
		c.mu.Unlock()
		return Ack{}, codeError(ErrIdentityMismatch, ReasonAuthentication, false, nil)
	}
	current := c.active
	if c.hasCandidate {
		current = c.candidate
	}
	hasCurrent := c.hasActive || c.hasCandidate
	if hasCurrent {
		if snapshot.Generation < current.Generation {
			c.mu.Unlock()
			return Ack{}, codeError(ErrStaleGeneration, ReasonStaleGeneration, false, nil)
		}
		if snapshot.Generation == current.Generation {
			if snapshot.ContentHash == current.ContentHash {
				ack := c.makeAckLocked(AckSnapshot, AckDuplicate, snapshot)
				c.mu.Unlock()
				return ack, nil
			}
			c.mu.Unlock()
			return Ack{}, codeError(ErrContentHashMismatch, ReasonSnapshotRejected, false, nil)
		}
	}
	// A full newer snapshot supersedes the pending candidate. Serialize its
	// cleanup before staging the replacement; the ready active config stays.
	previousPrepared := c.prepared
	c.mu.Unlock()
	if err := c.abortPrepared(previousPrepared); err != nil {
		return Ack{}, codeError(ErrSnapshotRejected, ReasonSnapshotRejected, false, err)
	}
	c.mu.Lock()
	if c.hasCandidate {
		c.prepared = nil
		c.hasCandidate = false
		c.candidate = Snapshot{}
		c.needsSnapshot = !c.hasActive
		c.restoreStateLocked()
	}
	c.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return Ack{}, codeError(ErrCanceled, ReasonCanceled, true, err)
	}
	prepareCtx, cancelPrepare := context.WithTimeout(ctx, c.config.ApplyTimeout)
	prepared, err := c.config.Applier.PrepareSnapshot(prepareCtx, snapshot)
	prepareErr := prepareCtx.Err()
	cancelPrepare()
	if err != nil {
		abortErr := c.abortPrepared(prepared)
		if ctx.Err() != nil || prepareErr != nil {
			cause := ctx.Err()
			if cause == nil {
				cause = prepareErr
			}
			return Ack{}, codeError(ErrCanceled, ReasonCanceled, true, joinCleanup(cause, abortErr))
		}
		return Ack{}, codeError(ErrSnapshotRejected, ReasonSnapshotRejected, false, joinCleanup(err, abortErr))
	}
	if prepareErr != nil {
		abortErr := c.abortPrepared(prepared)
		return Ack{}, codeError(ErrCanceled, ReasonCanceled, true, joinCleanup(prepareErr, abortErr))
	}
	if prepared == nil {
		return Ack{}, codeError(ErrSnapshotRejected, ReasonSnapshotRejected, false, errors.New("applier returned nil prepared configuration"))
	}
	c.mu.Lock()
	if err := c.ensureActiveLocked(c.config.Clock.Now().UTC()); err != nil {
		c.mu.Unlock()
		return Ack{}, c.abortAndJoin(err, prepared)
	}
	if c.hasCandidate && c.prepared != nil {
		c.mu.Unlock()
		return Ack{}, c.abortAndJoin(codeError(ErrGenerationGap, ReasonGenerationGap, true, nil), prepared)
	}
	snapshot = bindSnapshot(snapshot, c.config.Hello.AccountID, c.config.Hello.ConnectorID, c.welcome.SessionID, c.config.Hello.ProcessGeneration)
	c.candidate = snapshot
	c.hasCandidate = true
	c.prepared = prepared
	c.needsSnapshot = false
	c.restoreStateLocked()
	ack := c.makeAckLocked(AckSnapshot, AckApplied, snapshot)
	c.mu.Unlock()
	return ack, nil
}

func (c *ClientSession) ApplyDelta(ctx context.Context, delta Delta) (Ack, error) {
	if c == nil || ctx == nil {
		return Ack{}, ErrInvalidInput
	}
	if err := delta.Validate(); err != nil {
		return Ack{}, codeError(ErrDeltaRejected, ReasonGenerationGap, false, err)
	}
	c.applyMu.Lock()
	defer c.applyMu.Unlock()
	c.mu.Lock()
	if err := c.ensureActiveLocked(c.config.Clock.Now().UTC()); err != nil {
		c.mu.Unlock()
		return Ack{}, err
	}
	if err := delta.ValidateBound(); err != nil || delta.TunnelID != c.config.Hello.TunnelID || delta.AccountID != c.config.Hello.AccountID || delta.ConnectorID != c.config.Hello.ConnectorID || delta.SessionID != c.welcome.SessionID || delta.ProcessGeneration != c.config.Hello.ProcessGeneration {
		c.mu.Unlock()
		return Ack{}, codeError(ErrIdentityMismatch, ReasonAuthentication, false, nil)
	}
	if !c.hasActive || c.needsSnapshot || c.hasCandidate {
		ack := c.makeAckLocked(AckDelta, AckSnapshotRequired, Snapshot{Generation: delta.Generation, ContentHash: delta.ContentHash})
		c.mu.Unlock()
		return ack, codeError(ErrSnapshotRequired, ReasonGenerationGap, true, nil)
	}
	if delta.PreviousGeneration == c.active.Generation && delta.PreviousContentHash == c.active.ContentHash {
		if delta.Generation != c.active.Generation+1 {
			c.needsSnapshot = true
			ack := c.makeAckLocked(AckDelta, AckSnapshotRequired, Snapshot{Generation: delta.Generation, ContentHash: delta.ContentHash})
			c.mu.Unlock()
			return ack, codeError(ErrGenerationGap, ReasonGenerationGap, true, nil)
		}
	} else if delta.Generation <= c.active.Generation {
		c.mu.Unlock()
		return Ack{}, codeError(ErrStaleGeneration, ReasonStaleGeneration, false, nil)
	} else {
		c.needsSnapshot = true
		ack := c.makeAckLocked(AckDelta, AckSnapshotRequired, Snapshot{Generation: delta.Generation, ContentHash: delta.ContentHash})
		c.mu.Unlock()
		return ack, codeError(ErrGenerationGap, ReasonGenerationGap, true, nil)
	}
	c.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return Ack{}, codeError(ErrCanceled, ReasonCanceled, true, err)
	}
	prepareCtx, cancelPrepare := context.WithTimeout(ctx, c.config.ApplyTimeout)
	prepared, err := c.config.Applier.PrepareDelta(prepareCtx, delta)
	prepareErr := prepareCtx.Err()
	cancelPrepare()
	if err != nil {
		abortErr := c.abortPrepared(prepared)
		if ctx.Err() != nil || prepareErr != nil {
			cause := ctx.Err()
			if cause == nil {
				cause = prepareErr
			}
			return Ack{}, codeError(ErrCanceled, ReasonCanceled, true, joinCleanup(cause, abortErr))
		}
		return Ack{}, codeError(ErrDeltaRejected, ReasonSnapshotRejected, false, joinCleanup(err, abortErr))
	}
	if prepareErr != nil {
		abortErr := c.abortPrepared(prepared)
		return Ack{}, codeError(ErrCanceled, ReasonCanceled, true, joinCleanup(prepareErr, abortErr))
	}
	if prepared == nil {
		return Ack{}, codeError(ErrDeltaRejected, ReasonSnapshotRejected, false, errors.New("applier returned nil prepared configuration"))
	}
	c.mu.Lock()
	if err := c.ensureActiveLocked(c.config.Clock.Now().UTC()); err != nil {
		c.mu.Unlock()
		return Ack{}, c.abortAndJoin(err, prepared)
	}
	if c.hasCandidate && c.prepared != nil {
		c.mu.Unlock()
		return Ack{}, c.abortAndJoin(codeError(ErrGenerationGap, ReasonGenerationGap, true, nil), prepared)
	}
	c.candidate = Snapshot{AccountID: c.config.Hello.AccountID, TunnelID: delta.TunnelID, ConnectorID: c.config.Hello.ConnectorID, Generation: delta.Generation, SessionID: c.welcome.SessionID, ProcessGeneration: c.config.Hello.ProcessGeneration, ContentHash: delta.ContentHash, Payload: append([]byte(nil), delta.Payload...)}
	c.hasCandidate = true
	c.prepared = prepared
	c.restoreStateLocked()
	ack := c.makeAckLocked(AckDelta, AckApplied, c.candidate)
	c.mu.Unlock()
	return ack, nil
}

// RejectionAck binds a negative apply result to the authenticated session.
// Callers use it only after the incoming payload has passed structural and
// bound validation, so the generation/hash remain useful to the peer even
// when preparation failed before ClientSession could create its candidate.
func (c *ClientSession) RejectionAck(kind AckKind, generation uint64, contentHash string, code Code) Ack {
	if c == nil {
		return Ack{}
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return Ack{
		AccountID:         c.config.Hello.AccountID,
		TunnelID:          c.config.Hello.TunnelID,
		ConnectorID:       c.config.Hello.ConnectorID,
		SessionID:         c.welcome.SessionID,
		ProcessGeneration: c.config.Hello.ProcessGeneration,
		Kind:              kind,
		Status:            AckRejected,
		Generation:        generation,
		ContentHash:       contentHash,
		Code:              code,
	}
}

func (c *ClientSession) MarkReady(edgeReady, routeReady, originReady bool) (Readiness, error) {
	if c == nil {
		return Readiness{}, ErrInvalidInput
	}
	ctx, cancel := context.WithTimeout(context.Background(), c.config.ApplyTimeout)
	defer cancel()
	return c.MarkReadyContext(ctx, edgeReady, routeReady, originReady)
}

// MarkReadyContext activates the staged candidate only after all readiness
// dimensions are true. The previous active snapshot remains untouched when
// activation or readiness fails.
func (c *ClientSession) MarkReadyContext(ctx context.Context, edgeReady, routeReady, originReady bool) (Readiness, error) {
	if c == nil {
		return Readiness{}, ErrInvalidInput
	}
	if ctx == nil {
		return Readiness{}, ErrInvalidInput
	}
	c.applyMu.Lock()
	defer c.applyMu.Unlock()
	c.mu.Lock()
	if err := c.ensureActiveLocked(c.config.Clock.Now().UTC()); err != nil {
		c.mu.Unlock()
		return Readiness{}, err
	}
	if !c.hasCandidate {
		c.mu.Unlock()
		return Readiness{}, codeError(ErrSnapshotRequired, ReasonGenerationGap, true, nil)
	}
	readiness := Readiness{AccountID: c.config.Hello.AccountID, SessionID: c.welcome.SessionID, TunnelID: c.config.Hello.TunnelID, ConnectorID: c.config.Hello.ConnectorID, ProcessGeneration: c.config.Hello.ProcessGeneration, Generation: c.candidate.Generation, ContentHash: c.candidate.ContentHash, EdgeReady: edgeReady, RouteReady: routeReady, OriginReady: originReady}
	if !edgeReady || !routeReady || !originReady {
		c.mu.Unlock()
		return readiness, codeError(ErrNotReady, ReasonSnapshotRejected, true, nil)
	}
	prepared := c.prepared
	candidate := cloneSnapshot(c.candidate)
	c.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return readiness, codeError(ErrCanceled, ReasonCanceled, true, err)
	}
	if prepared == nil {
		return readiness, codeError(ErrSnapshotRejected, ReasonSnapshotRejected, false, errors.New("missing prepared configuration"))
	}
	if err := prepared.Activate(ctx); err != nil {
		abortErr := c.abortPrepared(prepared)
		c.mu.Lock()
		if c.hasCandidate && c.candidate.Generation == candidate.Generation && c.candidate.ContentHash == candidate.ContentHash {
			c.candidate = Snapshot{}
			c.hasCandidate = false
			c.prepared = nil
			c.needsSnapshot = true
			c.restoreStateLocked()
		}
		c.mu.Unlock()
		if ctx.Err() != nil {
			return readiness, codeError(ErrCanceled, ReasonCanceled, true, joinCleanup(err, abortErr))
		}
		return readiness, codeError(ErrSnapshotRejected, ReasonSnapshotRejected, false, joinCleanup(err, abortErr))
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.ensureActiveLocked(c.config.Clock.Now().UTC()); err != nil {
		return readiness, err
	}
	if !c.hasCandidate || c.candidate.Generation != candidate.Generation || c.candidate.ContentHash != candidate.ContentHash {
		return readiness, codeError(ErrSessionConflict, ReasonStaleGeneration, true, nil)
	}
	c.active = cloneSnapshot(c.candidate)
	c.hasActive = true
	c.candidate = Snapshot{}
	c.hasCandidate = false
	c.prepared = nil
	c.needsSnapshot = false
	c.readyGeneration = c.active.Generation
	c.state = SessionReady
	return readiness, nil
}

func (c *ClientSession) Heartbeat(now time.Time) (Heartbeat, error) {
	if c == nil {
		return Heartbeat{}, ErrInvalidInput
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if now.IsZero() {
		now = c.config.Clock.Now().UTC()
	}
	if err := c.ensureActiveLocked(now); err != nil {
		return Heartbeat{}, err
	}
	// Prefer the last promoted generation during a cutover. During bootstrap,
	// before the first candidate is promoted, report the exact staged candidate
	// so the server can renew this session while carrier/route/origin readiness
	// is still pending. This is liveness only and cannot activate the candidate.
	current, ok := c.currentCandidateLocked()
	if !ok {
		return Heartbeat{}, codeError(ErrSnapshotRequired, ReasonGenerationGap, true, nil)
	}
	return Heartbeat{AccountID: c.config.Hello.AccountID, SessionID: c.welcome.SessionID, TunnelID: c.config.Hello.TunnelID, ConnectorID: c.config.Hello.ConnectorID, ProcessGeneration: c.config.Hello.ProcessGeneration, LastAppliedGeneration: current.Generation, LastAppliedHash: current.ContentHash, SentAt: now}, nil
}

func (c *ClientSession) AcceptHeartbeatAck(ack HeartbeatAck) error {
	if c == nil {
		return ErrInvalidInput
	}
	now := c.config.Clock.Now().UTC()
	if err := ack.Validate(now); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.ensureActiveLocked(now); err != nil {
		return err
	}
	if ack.AccountID != c.config.Hello.AccountID || ack.TunnelID != c.config.Hello.TunnelID || ack.ConnectorID != c.config.Hello.ConnectorID || ack.SessionID != c.welcome.SessionID || ack.ProcessGeneration != c.config.Hello.ProcessGeneration {
		return codeError(ErrIdentityMismatch, ReasonAuthentication, false, nil)
	}
	c.lease.ExpiresAt = ack.LeaseExpiresAt
	c.lastHeartbeatAck = now
	return nil
}

// HandleDrain accepts a generation-bound drain request and immediately stops
// new streams through the runtime adapter's drain hook. It returns an
// acknowledgement even when no streams are active; completion is sent through
// DrainProgress so the server can distinguish acceptance from completion.
func (c *ClientSession) HandleDrain(ctx context.Context, request Drain) (DrainAck, error) {
	if c == nil || ctx == nil {
		return DrainAck{}, ErrInvalidInput
	}
	if err := ctx.Err(); err != nil {
		return DrainAck{}, codeError(ErrCanceled, ReasonCanceled, true, err)
	}
	now := c.config.Clock.Now().UTC()
	if err := request.Validate(now); err != nil {
		return DrainAck{}, err
	}
	c.mu.Lock()
	if err := c.ensureActiveLocked(now); err != nil {
		c.mu.Unlock()
		return DrainAck{}, err
	}
	if !hasCapability(c.welcome.Capabilities, CapabilityDrain) {
		c.mu.Unlock()
		return DrainAck{}, codeError(ErrCapabilityMissing, ReasonCapabilityMissing, false, nil)
	}
	if request.AccountID != c.config.Hello.AccountID || request.TunnelID != c.config.Hello.TunnelID || request.ConnectorID != c.config.Hello.ConnectorID || request.SessionID != c.welcome.SessionID || request.ProcessGeneration != c.config.Hello.ProcessGeneration {
		ack := c.makeDrainAckLocked(request, DrainRejected, 0, false, CodeDrainRejected)
		c.mu.Unlock()
		return ack, codeError(ErrIdentityMismatch, ReasonAuthentication, false, nil)
	}
	if c.drain != nil {
		if request.DrainID != c.drain.DrainID {
			ack := c.makeDrainAckLocked(request, DrainRejected, c.activeStreams, false, CodeDrainRejected)
			c.mu.Unlock()
			return ack, codeError(ErrSessionConflict, ReasonStaleGeneration, true, nil)
		}
		if request.Generation != c.drain.Generation || request.ContentHash != c.drain.ContentHash {
			ack := c.makeDrainAckLocked(request, DrainRejected, c.activeStreams, false, CodeDrainRejected)
			c.mu.Unlock()
			return ack, codeError(ErrContentHashMismatch, ReasonSnapshotRejected, false, nil)
		}
		ack := c.makeDrainAckLocked(request, c.drainStatus, c.activeStreams, c.drainStatus == DrainForced, c.drainCode)
		c.mu.Unlock()
		return ack, nil
	}
	if !c.hasActive || c.needsSnapshot || c.readyGeneration != c.active.Generation {
		ack := c.makeDrainAckLocked(request, DrainRejected, 0, false, CodeDrainRejected)
		c.mu.Unlock()
		return ack, codeError(ErrNotReady, ReasonSnapshotRejected, true, nil)
	}
	if request.Generation != c.active.Generation || request.ContentHash != c.active.ContentHash {
		ack := c.makeDrainAckLocked(request, DrainRejected, 0, false, CodeDrainRejected)
		c.mu.Unlock()
		return ack, codeError(ErrStaleGeneration, ReasonStaleGeneration, true, nil)
	}
	c.drain = cloneDrain(request)
	c.drainStatus = DrainAccepted
	c.drainCode = ""
	c.drainCompleted = false
	c.activeStreams = 0
	c.state = SessionDraining
	c.mu.Unlock()

	// Stop admission before observing the stream count. A deadline-bound
	// context prevents an unhealthy runtime from holding the control loop.
	stopCtx, cancelStop := context.WithDeadline(ctx, request.Deadline)
	stopErr := c.config.Drainer.StopNewStreams(stopCtx)
	cancelStop()
	if stopErr != nil {
		c.mu.Lock()
		if c.drain != nil && c.drain.DrainID == request.DrainID {
			c.drainStatus = DrainRejected
			c.drainCode = CodeDrainRejected
			c.drainCompleted = true
			c.restoreStateLocked()
		}
		ack := c.makeDrainAckLocked(request, DrainRejected, c.activeStreams, false, CodeDrainRejected)
		c.mu.Unlock()
		return ack, codeError(ErrDrainRejected, ReasonSnapshotRejected, true, stopErr)
	}
	countCtx, cancelCount := context.WithTimeout(ctx, c.config.AbortTimeout)
	activeStreams, countErr := c.config.Drainer.ActiveStreams(countCtx)
	cancelCount()
	if countErr != nil || activeStreams > MaxActiveStreams {
		if countErr == nil {
			countErr = ErrInvalidInput
		}
		c.mu.Lock()
		if c.drain != nil && c.drain.DrainID == request.DrainID {
			c.drainStatus = DrainRejected
			c.drainCode = CodeDrainRejected
			c.drainCompleted = true
			c.restoreStateLocked()
		}
		ack := c.makeDrainAckLocked(request, DrainRejected, 0, false, CodeDrainRejected)
		c.mu.Unlock()
		return ack, codeError(ErrDrainRejected, ReasonSnapshotRejected, true, countErr)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.drain == nil || c.drain.DrainID != request.DrainID {
		return c.makeDrainAckLocked(request, DrainRejected, activeStreams, false, CodeDrainRejected), codeError(ErrStaleSession, ReasonStaleGeneration, false, nil)
	}
	c.activeStreams = activeStreams
	return c.makeDrainAckLocked(request, DrainAccepted, activeStreams, false, ""), nil
}

// DrainProgress reports the number of streams that remain after the runtime
// has stopped admitting new streams. Completion and forced-close are distinct
// terminal outcomes and are idempotent for the active drain ID.
func (c *ClientSession) DrainProgress(ctx context.Context, activeStreams uint32, forcedClose bool) (DrainAck, error) {
	if c == nil || ctx == nil {
		return DrainAck{}, ErrInvalidInput
	}
	if activeStreams > MaxActiveStreams {
		return DrainAck{}, ErrInvalidInput
	}
	if err := ctx.Err(); err != nil {
		return DrainAck{}, codeError(ErrCanceled, ReasonCanceled, true, err)
	}
	now := c.config.Clock.Now().UTC()
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.ensureActiveLocked(now); err != nil {
		return DrainAck{}, err
	}
	if c.drain == nil {
		return DrainAck{}, codeError(ErrSessionConflict, ReasonStaleGeneration, false, nil)
	}
	c.activeStreams = activeStreams
	if c.drainCompleted {
		return c.makeDrainAckLocked(*c.drain, c.drainStatus, c.activeStreams, c.drainStatus == DrainForced, c.drainCode), nil
	}
	if forcedClose {
		if activeStreams != 0 {
			return DrainAck{}, codeError(ErrInvalidInput, ReasonSnapshotRejected, false, errors.New("forced drain must have zero active streams"))
		}
		c.drainStatus = DrainForced
		c.drainCode = CodeDrainTimeout
		c.drainCompleted = true
		return c.makeDrainAckLocked(*c.drain, DrainForced, 0, true, CodeDrainTimeout), nil
	}
	if activeStreams == 0 {
		c.drainStatus = DrainCompleted
		c.drainCode = ""
		c.drainCompleted = true
		return c.makeDrainAckLocked(*c.drain, DrainCompleted, 0, false, ""), nil
	}
	if !now.Before(c.drain.Deadline) {
		return c.makeDrainAckLocked(*c.drain, DrainProgress, activeStreams, false, ""), codeError(ErrDrainTimeout, ReasonHeartbeatTimeout, true, nil)
	}
	c.drainStatus = DrainProgress
	c.drainCode = ""
	return c.makeDrainAckLocked(*c.drain, DrainProgress, activeStreams, false, ""), nil
}

// CompleteDrain drives the runtime hook and emits one terminal acknowledgement.
// It is the preferred adapter entry point because stream counts and forced
// closure stay behind the bounded Drainer interface.
func (c *ClientSession) CompleteDrain(ctx context.Context, force bool) (DrainAck, error) {
	if c == nil || ctx == nil {
		return DrainAck{}, ErrInvalidInput
	}
	c.mu.RLock()
	request := c.drain
	completed := c.drainCompleted
	status := c.drainStatus
	code := c.drainCode
	active := c.activeStreams
	c.mu.RUnlock()
	if request == nil {
		return DrainAck{}, codeError(ErrSessionConflict, ReasonStaleGeneration, false, nil)
	}
	if completed {
		c.mu.RLock()
		ack := c.makeDrainAckLocked(*request, status, active, status == DrainForced, code)
		c.mu.RUnlock()
		return ack, nil
	}
	if force {
		forceCtx, cancel := context.WithTimeout(ctx, c.config.AbortTimeout)
		err := c.config.Drainer.ForceClose(forceCtx)
		cancel()
		if err != nil {
			return DrainAck{}, codeError(ErrDrainRejected, ReasonSnapshotRejected, true, err)
		}
		return c.DrainProgress(ctx, 0, true)
	}
	countCtx, cancel := context.WithTimeout(ctx, c.config.AbortTimeout)
	active, err := c.config.Drainer.ActiveStreams(countCtx)
	cancel()
	if err != nil {
		return DrainAck{}, codeError(ErrDrainRejected, ReasonSnapshotRejected, true, err)
	}
	return c.DrainProgress(ctx, active, false)
}

func (c *ClientSession) makeDrainAckLocked(request Drain, status DrainStatus, activeStreams uint32, forced bool, code Code) DrainAck {
	return DrainAck{AccountID: c.config.Hello.AccountID, TunnelID: c.config.Hello.TunnelID, ConnectorID: c.config.Hello.ConnectorID, SessionID: c.welcome.SessionID, ProcessGeneration: c.config.Hello.ProcessGeneration, DrainID: request.DrainID, Generation: request.Generation, ContentHash: request.ContentHash, Status: status, ActiveStreams: activeStreams, ForcedClose: forced, Code: code}
}

func cloneDrain(request Drain) *Drain {
	copy := request
	return &copy
}

func (c *ClientSession) Drain() (Drain, DrainStatus, uint32, bool) {
	if c == nil {
		return Drain{}, "", 0, false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.drain == nil {
		return Drain{}, "", 0, false
	}
	return *c.drain, c.drainStatus, c.activeStreams, c.drainCompleted
}

func (c *ClientSession) RenewalRequest(now time.Time, nonce, signedProof string) (RenewalRequest, error) {
	if c == nil {
		return RenewalRequest{}, ErrInvalidInput
	}
	if now.IsZero() {
		now = c.config.Clock.Now().UTC()
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if err := c.ensureActiveReadLocked(now); err != nil {
		return RenewalRequest{}, err
	}
	request := RenewalRequest{SessionID: c.welcome.SessionID, AccountID: c.auth.AccountID, TunnelID: c.auth.TunnelID, ConnectorID: c.auth.ConnectorID, HostID: c.auth.HostID, IdentityKeyID: c.auth.IdentityKeyID, IdentityKeyThumbprint: c.auth.IdentityKeyThumbprint, ProcessGeneration: c.config.Hello.ProcessGeneration, CredentialGeneration: c.auth.CredentialGeneration, Nonce: nonce, SignedProof: signedProof, RequestedAt: now}
	if err := request.Validate(); err != nil {
		return RenewalRequest{}, err
	}
	return request, nil
}

func (c *ClientSession) ensureActiveReadLocked(now time.Time) error {
	if c.state == SessionClosed {
		return codeError(ErrSessionClosed, c.disconnectReason, false, nil)
	}
	if c.state == SessionNew {
		return codeError(ErrSessionConflict, ReasonProtocolClosed, false, nil)
	}
	if !c.lease.ExpiresAt.After(now) {
		return codeError(ErrLeaseExpired, ReasonLeaseExpired, true, nil)
	}
	return nil
}

func (c *ClientSession) restoreStateLocked() {
	if c.hasActive && c.readyGeneration == c.active.Generation && !c.needsSnapshot {
		c.state = SessionReady
		return
	}
	if c.hasCandidate {
		c.state = SessionAwaitingReady
		return
	}
	c.state = SessionAwaitingSnapshot
}

func (c *ClientSession) abortPrepared(prepared PreparedConfig) error {
	if prepared == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), c.config.AbortTimeout)
	defer cancel()
	return prepared.Abort(ctx)
}

func joinCleanup(primary, cleanup error) error {
	if cleanup == nil {
		return primary
	}
	cleanup = fmt.Errorf("abort prepared configuration: %w", cleanup)
	if primary == nil {
		return cleanup
	}
	return errors.Join(primary, cleanup)
}

func (c *ClientSession) abortAndJoin(primary error, prepared PreparedConfig) error {
	return joinCleanup(primary, c.abortPrepared(prepared))
}

func (c *ClientSession) ApplyRenewal(result AuthResult) error {
	if c == nil {
		return ErrInvalidInput
	}
	now := c.config.Clock.Now().UTC()
	if err := result.ValidateBound(); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.ensureActiveLocked(now); err != nil {
		return err
	}
	if result.AccountID != c.auth.AccountID || result.TunnelID != c.auth.TunnelID || result.ConnectorID != c.auth.ConnectorID || result.SessionID != c.welcome.SessionID || result.HostID != c.auth.HostID || result.IdentityKeyID != c.auth.IdentityKeyID || result.IdentityKeyThumbprint != c.auth.IdentityKeyThumbprint || result.ProcessGeneration != c.config.Hello.ProcessGeneration || result.CredentialGeneration < c.auth.CredentialGeneration {
		return codeError(ErrIdentityMismatch, ReasonAuthentication, false, nil)
	}
	if result.CredentialExpiresAt.Before(c.auth.ExpiresAt) || result.LeaseExpiresAt.Before(c.lease.ExpiresAt) {
		return codeError(ErrSessionConflict, ReasonAuthentication, false, nil)
	}
	if result.CredentialGeneration == c.auth.CredentialGeneration && result.CredentialExpiresAt.Equal(c.auth.ExpiresAt) && result.LeaseExpiresAt.Equal(c.lease.ExpiresAt) {
		return nil
	}
	c.auth = AuthRequest{AccountID: result.AccountID, TunnelID: result.TunnelID, ConnectorID: result.ConnectorID, HostID: result.HostID, IdentityKeyID: result.IdentityKeyID, IdentityKeyThumbprint: result.IdentityKeyThumbprint, ProcessGeneration: result.ProcessGeneration, CredentialGeneration: result.CredentialGeneration, IssuedAt: now, ExpiresAt: result.CredentialExpiresAt}
	c.lease.ExpiresAt = result.LeaseExpiresAt
	c.lastHeartbeatAck = now
	return nil
}

func (c *ClientSession) Applied() (Snapshot, bool) {
	if c == nil {
		return Snapshot{}, false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	snapshot, ok := c.currentCandidateLocked()
	if !ok {
		return Snapshot{}, false
	}
	snapshot.Payload = append([]byte(nil), snapshot.Payload...)
	return snapshot, true
}

// Candidate returns the snapshot staged by the most recent accepted apply but
// not yet promoted by MarkReadyContext. A control transport must use this
// exact value when asking the runtime to probe routes and origins; Applied may
// still refer to the previously active snapshot during a cutover.
func (c *ClientSession) Candidate() (Snapshot, bool) {
	if c == nil {
		return Snapshot{}, false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if !c.hasCandidate {
		return Snapshot{}, false
	}
	return cloneSnapshot(c.candidate), true
}

func (c *ClientSession) currentCandidateLocked() (Snapshot, bool) {
	if c.hasActive {
		return c.active, true
	}
	if c.hasCandidate {
		return c.candidate, true
	}
	return Snapshot{}, false
}

// Active returns the last promoted, ready configuration. A staged candidate
// is deliberately not returned as active until MarkReady succeeds.
func (c *ClientSession) Active() (Snapshot, bool) {
	if c == nil {
		return Snapshot{}, false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if !c.hasActive {
		return Snapshot{}, false
	}
	snapshot := cloneSnapshot(c.active)
	return snapshot, true
}

func (c *ClientSession) State() SessionState {
	if c == nil {
		return SessionClosed
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.state
}

func (c *ClientSession) Close(reason DisconnectReason) error {
	if c == nil || !validDisconnectReason(reason) {
		return ErrInvalidInput
	}
	c.applyMu.Lock()
	defer c.applyMu.Unlock()
	c.mu.Lock()
	if c.state == SessionClosed {
		c.mu.Unlock()
		return nil
	}
	c.state = SessionClosed
	c.disconnectReason = reason
	prepared := c.prepared
	c.prepared = nil
	c.candidate = Snapshot{}
	c.hasCandidate = false
	c.mu.Unlock()
	if err := c.abortPrepared(prepared); err != nil {
		return codeError(ErrSnapshotRejected, ReasonSnapshotRejected, false, joinCleanup(nil, err))
	}
	return nil
}

func (c *ClientSession) Disconnect() Disconnect {
	if c == nil {
		return Disconnect{}
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	reason := c.disconnectReason
	if reason == "" {
		reason = ReasonProtocolClosed
	}
	return Disconnect{AccountID: c.config.Hello.AccountID, TunnelID: c.config.Hello.TunnelID, ConnectorID: c.config.Hello.ConnectorID, SessionID: c.welcome.SessionID, ProcessGeneration: c.config.Hello.ProcessGeneration, Reason: reason, Retryable: reason == ReasonLeaseExpired || reason == ReasonHeartbeatTimeout || reason == ReasonCredentialExpired || reason == ReasonSessionReplaced}
}

func (c *ClientSession) CheckLease(now time.Time) error {
	if c == nil {
		return ErrInvalidInput
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ensureActiveLocked(now)
}

func (c *ClientSession) makeAckLocked(kind AckKind, status AckStatus, snapshot Snapshot) Ack {
	return Ack{AccountID: c.config.Hello.AccountID, TunnelID: c.config.Hello.TunnelID, ConnectorID: c.config.Hello.ConnectorID, SessionID: c.welcome.SessionID, ProcessGeneration: c.config.Hello.ProcessGeneration, Kind: kind, Status: status, Generation: snapshot.Generation, ContentHash: snapshot.ContentHash}
}

type noopApplier struct{}

func (noopApplier) PrepareSnapshot(context.Context, Snapshot) (PreparedConfig, error) {
	return noopPrepared{}, nil
}
func (noopApplier) PrepareDelta(context.Context, Delta) (PreparedConfig, error) {
	return noopPrepared{}, nil
}

type noopPrepared struct{}

func (noopPrepared) Activate(context.Context) error { return nil }
func (noopPrepared) Abort(context.Context) error    { return nil }
