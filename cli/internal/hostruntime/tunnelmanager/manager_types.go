package tunnelmanager

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/connector"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hoststate"
)

var (
	ErrInvalidConfig        = errors.New("invalid tunnel manager configuration")
	ErrNotStarted           = errors.New("tunnel manager not started")
	ErrAlreadyStarted       = errors.New("tunnel manager already started")
	ErrGenerationConflict   = errors.New("tunnel generation conflict")
	ErrConnectorUnavailable = errors.New("tunnel connector unavailable")
	ErrOriginUnavailable    = errors.New("tunnel origin unavailable")
)

type networkGenerationContextKey struct{}

// networkGenerationContext identifies a pure network reattachment. Network
// generations are deliberately separate from config generations and are
// scoped to one durable tunnel so an event for one tunnel cannot fence an
// unrelated tunnel's replacement.
type networkGenerationContext struct {
	TunnelID   string
	Generation uint64
}

type networkRecoveryState struct {
	Observed uint64
	Applied  uint64
	InFlight uint64
}

const (
	CodeReady                = "ready"
	CodeReattached           = "reattached"
	CodeNetworkReattached    = "network_reattached"
	CodeOriginUnavailable    = "origin_unavailable"
	CodeControlUnavailable   = "control_unavailable"
	CodeConnectorUnavailable = "connector_unavailable"
	CodeApplyFailed          = "apply_failed"
	CodePersistenceConflict  = "persistence_conflict"
	CodePaused               = "paused"
	CodeStopped              = "stopped"
)

type StateStore interface {
	Snapshot() (hoststate.State, uint64, error)
	Commit(expectedRevision uint64, next hoststate.State) (uint64, error)
}

// Factory stages a connector/runtime generation without changing active
// traffic. The exact durable tunnel and connector identities are supplied on
// every call so a restart cannot allocate replacements implicitly.
type Factory interface {
	Prepare(context.Context, ApplyRequest) (Candidate, error)
}

type ApplyRequest struct {
	Tunnel    hoststate.Tunnel
	Connector hoststate.Connector
	Snapshot  hoststate.ConfigSnapshot
	Decoded   hoststate.TunnelConfigSnapshot
	Recovery  bool
}

// controlSessionBinding records the authenticated connector session which
// supplied a snapshot. It is separate from the durable config generation:
// reconnects may keep the same configuration while requiring a fresh carrier
// bound to a new server session/process epoch.
type controlSessionBinding struct {
	AccountID         string
	ConnectorID       string
	SessionID         string
	ProcessGeneration uint64
	Generation        uint64
	ContentHash       string
}

type ProbeResult struct {
	Ready         bool
	FailureCode   string
	HealthyRoutes []string
	FailedRoutes  []string
}

// Candidate is fully isolated from the active generation until Activate.
// ProbeOrigins must be safe and non-mutating. Activate makes the prepared
// generation eligible while retaining the prior Active until the manager
// commits durable LKG state and explicitly drains it.
type Candidate interface {
	ProbeOrigins(context.Context) (ProbeResult, error)
	Activate(context.Context) (Active, error)
	Abort(context.Context) error
}

type Active interface {
	TunnelID() string
	ConnectorID() string
	Generation() uint64
	ContentHash() string
	Drain(context.Context) error
	Close(context.Context) error
}

// ActiveCarrierProvider exposes the authenticated data-carrier handle owned by
// one active runtime. It is intentionally optional so test and non-network
// candidates do not need to depend on connector transport details.
type ActiveCarrierProvider interface {
	ActiveDataCarrier() *connector.ActiveDataCarrier
}

// ActiveChange is emitted after the manager publishes a new active runtime or
// removes one. Previous and Current are immutable handles for the callback;
// the manager remains their lifecycle owner. A callback must not retain either
// handle after it receives a subsequent change without its own fencing.
type ActiveChange struct {
	TunnelID string
	Previous Active
	Current  Active
}

type Observation struct {
	TunnelID          string
	ConnectorID       string
	DesiredGeneration uint64
	AppliedGeneration uint64
	Code              string
	Retryable         bool
	NextRetryAt       time.Time
	ObservedAt        time.Time
	Err               error
}

type Config struct {
	Store   StateStore
	Factory Factory
	HostID  string
	// Refresh asks the control client to place newer authoritative desired
	// state in Store. A transient failure never removes cached desired/LKG.
	Refresh           func(context.Context) error
	Clock             func() time.Time
	ReconcileInterval time.Duration
	ApplyTimeout      time.Duration
	DrainTimeout      time.Duration
	// Report must return promptly. It runs on the serialized reconciliation
	// path so observation order matches durable generation order.
	Report         func(Observation)
	ActiveObserver func(ActiveChange)
	// InspectorPurge revokes daemon-local inspector captures for routes that
	// stopped forwarding (removed/disabled routes, paused or deleted
	// tunnels). It runs on the reconciliation path and must return promptly
	// like Report. Nil disables purge notification; captures then expire by
	// retention and replay stays fenced by authority lifetime.
	InspectorPurge func(routeIDs []string)
}

type Manager struct {
	config Config

	lifecycleMu  sync.Mutex
	activationMu sync.Mutex
	opMu         sync.Mutex
	mu           sync.RWMutex
	ctx          context.Context
	stop         context.CancelFunc
	done         chan struct{}

	started bool
	closed  bool
	active  map[string]Active
	seen    map[string]uint64
	// sessionBindings is the latest accepted connector-v1 session per tunnel.
	// It is an in-memory liveness fence only; desired/LKG state remains in the
	// durable store.
	sessionBindings map[string]controlSessionBinding
	wake            chan struct{}
	// networkGeneration is the highest host-wide event observed for legacy
	// callers. networkStates is the authoritative per-tunnel fence. Keeping the
	// host-wide watermark preserves the old monitor API while allowing each
	// durable tunnel to apply the same event independently.
	networkGeneration uint64
	networkStates     map[string]networkRecoveryState
	// retries holds, under opMu, the backoff of tunnels whose last scheduled
	// reconcile failed, so a tunnel the control plane keeps refusing (for example
	// one that was deleted) is not retried every tick.
	retries map[string]reconcileRetry

	shutdownDone chan struct{}
	shutdownErr  error
}
