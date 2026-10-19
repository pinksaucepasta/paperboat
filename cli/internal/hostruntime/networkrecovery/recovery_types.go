package networkrecovery

import (
	"context"
	"errors"
	"time"
)

var (
	ErrInvalidConfiguration = errors.New("invalid network recovery configuration")
	ErrInvalidObservation   = errors.New("invalid network recovery observation")
	ErrStopped              = errors.New("network recovery stopped")
	ErrNoPendingChange      = errors.New("no pending network change")
	ErrReplacementNotReady  = errors.New("network carrier replacement was not ready")
	ErrIdentityChanged      = errors.New("network carrier identity changed")

	// These sentinel errors let platform and transport code classify failures
	// without coupling the controller to a particular carrier implementation.
	ErrAuthentication    = errors.New("network carrier authentication failed")
	ErrConfiguration     = errors.New("network carrier configuration failed")
	ErrDNS               = errors.New("network carrier DNS failed")
	ErrOrigin            = errors.New("network carrier origin failed")
	ErrEdge              = errors.New("network carrier edge failed")
	ErrLocalSystem       = errors.New("network carrier local system failed")
	ErrTimeout           = errors.New("network carrier operation timed out")
	ErrSourceUnavailable = errors.New("network observation source unavailable")
)

// Reason identifies a network input that can invalidate an outbound carrier.
// Reasons are intentionally coarse and contain no interface names, addresses,
// hostnames, proxy URLs, or other sensitive network details.
type Reason uint16

const (
	ReasonDefaultRoute Reason = 1 << iota
	ReasonInterfaceAddress
	ReasonDNS
	ReasonProxy
	ReasonPathViability
	ReasonSleepWake
)

const allReasons = ReasonDefaultRoute | ReasonInterfaceAddress | ReasonDNS | ReasonProxy | ReasonPathViability | ReasonSleepWake

// Observation is a privacy-preserving network change notification. Generation
// is monotonic for the host installation and fences stale callbacks.
type Observation struct {
	Generation uint64
	Reasons    Reason
	Viable     bool
	At         time.Time
}

func (o Observation) Validate() error {
	if o.Generation == 0 || o.Reasons == 0 || o.Reasons&^allReasons != 0 {
		return ErrInvalidObservation
	}
	return nil
}

// Identity is the durable logical identity that must survive carrier
// replacement. Session/process and network generations do not belong here.
type Identity struct {
	EnvironmentID string
	MachineID     string
	TunnelID      string
	ConnectorID   string
}

func (i Identity) Validate() error {
	if i.EnvironmentID == "" || i.MachineID == "" || i.TunnelID == "" || i.ConnectorID == "" {
		return ErrInvalidConfiguration
	}
	return nil
}

// ReplacementRequest is immutable input for one staged carrier attempt.
type ReplacementRequest struct {
	Identity                  Identity
	NetworkGeneration         uint64
	PreviousNetworkGeneration uint64
	Reasons                   Reason
	Attempt                   uint32
}

func (r ReplacementRequest) Validate() error {
	if err := r.Identity.Validate(); err != nil || r.NetworkGeneration == 0 || r.Reasons == 0 || r.Reasons&^allReasons != 0 || r.Attempt == 0 {
		return ErrInvalidConfiguration
	}
	return nil
}

// ReplacementResult is returned only after the new carrier has passed
// authenticated readiness. CarrierGeneration is the carrier/config generation
// and is kept separate from NetworkGeneration.
type ReplacementResult struct {
	Identity          Identity
	NetworkGeneration uint64
	CarrierGeneration uint64
	Ready             bool
}

func (r ReplacementResult) Validate() error {
	if err := r.Identity.Validate(); err != nil || r.NetworkGeneration == 0 || r.CarrierGeneration == 0 {
		return ErrInvalidConfiguration
	}
	if !r.Ready {
		return ErrReplacementNotReady
	}
	return nil
}

// CarrierReplacer stages and activates a new carrier. Implementations must
// preserve the old carrier until the returned result is ready and must fence a
// request whose NetworkGeneration is stale.
type CarrierReplacer interface {
	Replace(context.Context, ReplacementRequest) (ReplacementResult, error)
}

// EventSource adapts a platform network monitor to the recovery controller.
// Start returns a bounded stream of privacy-preserving observations. Close is
// idempotent and must not retain callbacks after it returns.
type EventSource interface {
	Start(context.Context) (<-chan Observation, error)
	Close() error
}

// Timer is the small timer surface needed by the controller. It makes debounce,
// retry, and stable-ready tests deterministic with a fake clock.
type Timer interface {
	C() <-chan time.Time
	Stop() bool
	Reset(time.Duration) bool
}

// Clock is injectable so retry scheduling and stability windows can be tested
// without sleeping.
type Clock interface {
	Now() time.Time
	NewTimer(time.Duration) Timer
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now().UTC() }

func (realClock) NewTimer(d time.Duration) Timer { return realTimer{timer: time.NewTimer(d)} }

type realTimer struct{ timer *time.Timer }

func (t realTimer) C() <-chan time.Time { return t.timer.C }

func (t realTimer) Stop() bool { return t.timer.Stop() }

func (t realTimer) Reset(d time.Duration) bool { return t.timer.Reset(d) }

// HealthState is the typed network recovery projection.
type HealthState string

const (
	StateUnknown  HealthState = "unknown"
	StateReady    HealthState = "ready"
	StateDegraded HealthState = "degraded"
	StateDown     HealthState = "down"
	StateStopped  HealthState = "stopped"
)

// HealthSnapshot contains only stable codes and scheduling metadata. It never
// exposes the underlying error, endpoint, address, credential, or network
// fingerprint.
type HealthSnapshot struct {
	State                   HealthState  `json:"state"`
	Code                    string       `json:"code"`
	Failure                 FailureClass `json:"failure,omitempty"`
	Reason                  Reason       `json:"reason,omitempty"`
	NetworkGeneration       uint64       `json:"network_generation,omitempty"`
	ActiveNetworkGeneration uint64       `json:"active_network_generation,omitempty"`
	Attempt                 uint32       `json:"attempt,omitempty"`
	NextRetryAt             time.Time    `json:"next_retry_at,omitempty"`
	StableSince             time.Time    `json:"stable_since,omitempty"`
	CheckedAt               time.Time    `json:"checked_at"`
}

// NextRetry reports the exact scheduled retry, if one exists.
func (h HealthSnapshot) NextRetry() (time.Time, bool) {
	if h.NextRetryAt.IsZero() {
		return time.Time{}, false
	}
	return h.NextRetryAt, true
}

type Config struct {
	Identity          Identity
	Replacer          CarrierReplacer
	Source            EventSource
	Clock             Clock
	Debounce          time.Duration
	InitialBackoff    time.Duration
	MaxBackoff        time.Duration
	StableReadyWindow time.Duration
	Jitter            func(time.Duration) time.Duration
}

const (
	defaultDebounce          = 250 * time.Millisecond
	defaultInitialBackoff    = time.Second
	defaultMaxBackoff        = time.Minute
	defaultStableReadyWindow = 5 * time.Second
)
