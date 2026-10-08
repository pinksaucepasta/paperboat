package connector

import (
	"context"
	"errors"
	"math/rand/v2"
	"reflect"
	"sync"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/errorreport"
)

var ErrSupervisorInvalid = errors.New("invalid connector supervisor configuration")

// NetworkReason mirrors the platform network monitor's privacy-preserving
// reason bits without coupling the connector package to a specific OS monitor.
type NetworkReason uint16

const (
	NetworkReasonDefaultRoute NetworkReason = 1 << iota
	NetworkReasonInterfaceAddress
	NetworkReasonAddressFamily
	NetworkReasonProxy
	NetworkReasonNetworkCost
	NetworkReasonViability
	NetworkReasonWake
)

// NetworkChange is the typed input used by the runtime network handler. The
// generation fences stale callbacks and Rebind requests proactive replacement
// of the current carrier.
type NetworkChange struct {
	Generation uint64
	Reasons    NetworkReason
	Rebind     bool
	Viable     bool
}

type AdmissionSource interface {
	Admission(context.Context) (Admission, error)
}

type RetryWaiter interface {
	Wait(context.Context, time.Duration, <-chan struct{}) error
}

type timerWaiter struct{}

func (timerWaiter) Wait(ctx context.Context, delay time.Duration, wake <-chan struct{}) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-wake:
		return nil
	case <-timer.C:
		return nil
	}
}

type SupervisorConfig struct {
	Manager        *Manager
	Admissions     AdmissionSource
	InitialBackoff time.Duration
	MaxBackoff     time.Duration
	Jitter         float64
	RandomFloat    func() float64
	Waiter         RetryWaiter
	Metrics        interface {
		Record(string, float64, map[string]string) error
	}
}

type Supervisor struct {
	mu                sync.Mutex
	config            SupervisorConfig
	cancel            context.CancelFunc
	done              chan struct{}
	wake              chan struct{}
	routes            chan struct{}
	running           bool
	networkGeneration uint64
}

func NewSupervisor(config SupervisorConfig) (*Supervisor, error) {
	if config.InitialBackoff == 0 {
		config.InitialBackoff = time.Second
	}
	if config.MaxBackoff == 0 {
		config.MaxBackoff = time.Minute
	}
	if config.Jitter == 0 {
		config.Jitter = 0.2
	}
	if config.Waiter == nil {
		config.Waiter = timerWaiter{}
	}
	if config.RandomFloat == nil {
		config.RandomFloat = rand.Float64
	}
	if config.Manager == nil || config.Admissions == nil || config.InitialBackoff <= 0 || config.MaxBackoff < config.InitialBackoff || config.Jitter < 0 || config.Jitter > 1 {
		return nil, ErrSupervisorInvalid
	}
	return &Supervisor{config: config, wake: make(chan struct{}, 1), routes: make(chan struct{}, 1)}, nil
}

func (s *Supervisor) Start(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running {
		return ErrSupervisorInvalid
	}
	runCtx, cancel := context.WithCancel(ctx)
	s.cancel, s.done, s.running = cancel, make(chan struct{}), true
	go s.loop(runCtx)
	return nil
}

func (s *Supervisor) NetworkChanged() {
	s.NetworkChangedEvent(NetworkChange{Rebind: true})
}

// NetworkChangedEvent records a monotonic network generation, fences the
// manager's active carrier, and wakes both admission backoff and the active
// connection wait. A route signal is deliberately used for rebind events so
// recovery does not wait for a long TCP timeout.
func (s *Supervisor) NetworkChangedEvent(change NetworkChange) {
	if s == nil {
		return
	}
	if change.Generation != 0 {
		s.mu.Lock()
		if change.Generation <= s.networkGeneration {
			s.mu.Unlock()
			return
		}
		s.networkGeneration = change.Generation
		s.mu.Unlock()
		s.config.Manager.NetworkChanged(change.Generation)
	}
	select {
	case s.wake <- struct{}{}:
	default:
	}
	if change.Rebind {
		select {
		case s.routes <- struct{}{}:
		default:
		}
	}
}

// RoutesChanged requests a fresh connector admission because proxy ownership is
// part of the handoff.
func (s *Supervisor) RoutesChanged() {
	select {
	case s.routes <- struct{}{}:
	default:
	}
}

func (s *Supervisor) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	if !s.running {
		s.mu.Unlock()
		return nil
	}
	cancel, done := s.cancel, s.done
	s.mu.Unlock()
	cancel()
	select {
	case <-done:
		return s.config.Manager.Shutdown(ctx)
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Supervisor) loop(ctx context.Context) {
	defer func() { s.mu.Lock(); s.running = false; close(s.done); s.mu.Unlock() }()
	backoff := s.config.InitialBackoff
	var recoveryStarted time.Time
	failureReported := false
	observeFailure := func(stage, code string, err error) {
		if err == nil || onlyConnectorCancellation(err) || failureReported {
			return
		}
		errorreport.Current().ObserveFailure(ctx, "paperboat-daemon", "connector_admission", stage, code, err)
		failureReported = true
	}
	for ctx.Err() == nil {
		admission, err := s.config.Admissions.Admission(ctx)
		if err == nil {
			// QUIC probing is bounded separately, but TCP login and proxy
			// publication can legitimately take several seconds on a cold edge.
			// Keep the acceptance bound below the service readiness budget
			// while leaving enough time for that control exchange to settle.
			acceptCtx, cancelAccept := context.WithTimeout(ctx, 25*time.Second)
			result, acceptErr := s.config.Manager.Accept(acceptCtx, admission)
			cancelAccept()
			if acceptErr == nil {
				failureReported = false
				if !recoveryStarted.IsZero() && s.config.Metrics != nil {
					_ = s.config.Metrics.Record("paperboat_runtime_connector_recovery_seconds", time.Since(recoveryStarted).Seconds(), nil)
					recoveryStarted = time.Time{}
				}
				metricResult := "connected"
				if result.Replaced {
					metricResult = "replaced"
				}
				s.recordRetry(string(result.Transport), metricResult)
				backoff = s.config.InitialBackoff
				waitCtx, cancelWait := context.WithCancel(ctx)
				waitResult := make(chan error, 1)
				go func() { waitResult <- s.config.Manager.WaitDisconnected(waitCtx, result.Generation) }()
				var waitErr error
				select {
				case waitErr = <-waitResult:
				case <-s.routes:
					cancelWait()
					waitErr = <-waitResult
				}
				cancelWait()
				if onlyConnectorTermination(waitErr) && ctx.Err() == nil {
					continue
				}
				if waitErr != nil && ctx.Err() == nil {
					observeFailure("peer_connect", "transport_failed", waitErr)
					if s.config.Waiter.Wait(ctx, s.jitter(backoff), s.wake) != nil {
						return
					}
				}
				continue
			}
			if errors.Is(acceptErr, ErrShuttingDown) {
				return
			}
			var classified interface {
				DiagnosticStage() string
				DiagnosticCode() string
			}
			if errors.As(acceptErr, &classified) {
				observeFailure(classified.DiagnosticStage(), classified.DiagnosticCode(), acceptErr)
			} else {
				observeFailure("peer_authority", "peer_authority_failed", acceptErr)
			}
		} else {
			var observed interface{ DiagnosticObserved() bool }
			if !errors.As(err, &observed) || !observed.DiagnosticObserved() {
				observeFailure("peer_authority", "peer_authority_failed", err)
			}
		}
		s.recordRetry("none", "failed")
		if recoveryStarted.IsZero() {
			recoveryStarted = time.Now()
		}
		if s.config.Waiter.Wait(ctx, s.jitter(backoff), s.wake) != nil {
			return
		}
		backoff *= 2
		if backoff > s.config.MaxBackoff {
			backoff = s.config.MaxBackoff
		}
	}
}

func onlyConnectorCancellation(err error) bool {
	return connectorErrorLeaves(err, func(leaf error) bool {
		return leaf == context.Canceled || leaf == context.DeadlineExceeded
	})
}

func onlyConnectorTermination(err error) bool {
	return connectorErrorLeaves(err, func(leaf error) bool {
		return leaf == context.Canceled || leaf == context.DeadlineExceeded || leaf == ErrGenerationStale
	})
}

func connectorErrorLeaves(err error, expected func(error) bool) bool {
	if err == nil {
		return false
	}
	remaining := []error{err}
	seen := make(map[error]struct{})
	leaves := 0
	for visited := 0; len(remaining) > 0; visited++ {
		if visited >= 16 {
			return false
		}
		current := remaining[0]
		remaining = remaining[1:]
		if current == nil {
			return false
		}
		typeOf := reflect.TypeOf(current)
		if typeOf.Comparable() {
			if _, ok := seen[current]; ok {
				return false
			}
			seen[current] = struct{}{}
		}
		value := reflect.ValueOf(current)
		switch value.Kind() {
		case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
			if value.IsNil() {
				return false
			}
		}
		switch wrapped := current.(type) {
		case interface{ Unwrap() []error }:
			children := wrapped.Unwrap()
			if len(children) == 0 || len(children) > 16-visited-1 || len(remaining)+len(children) > 16-visited-1 {
				return false
			}
			remaining = append(remaining, children...)
		case interface{ Unwrap() error }:
			child := wrapped.Unwrap()
			if child == nil || len(remaining)+1 > 16-visited-1 {
				return false
			}
			remaining = append(remaining, child)
		default:
			leaves++
			if !expected(current) {
				return false
			}
		}
	}
	return leaves > 0
}

func (s *Supervisor) recordRetry(transport, result string) {
	if s.config.Metrics != nil {
		_ = s.config.Metrics.Record("paperboat_runtime_connector_retries_total", 1, map[string]string{"transport": safeTransportLabel(transport), "result": result})
	}
}

func safeTransportLabel(value string) string {
	switch Transport(value) {
	case Auto, QUIC, TCPDedicated, TCPMux:
		return value
	default:
		return "none"
	}
}

func (s *Supervisor) jitter(delay time.Duration) time.Duration {
	if s.config.Jitter == 0 {
		return delay
	}
	// math/rand is used only for scheduling. Admission and credential entropy are
	// supplied by their security-owning components.
	variation := (s.config.RandomFloat()*2 - 1) * s.config.Jitter
	value := time.Duration(float64(delay) * (1 + variation))
	if value < 0 {
		return 0
	}
	return value
}
