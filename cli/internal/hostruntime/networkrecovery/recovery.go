// Package networkrecovery owns the stable-host network recovery policy.
//
// The package deliberately separates observing a network change from replacing
// a carrier.  An observer may report route, address, resolver, proxy, path, or
// wake changes from any platform.  A replacer is responsible for staging a new
// authenticated carrier and only returning after it is ready.  The controller
// never mutates the active carrier itself, so a failed replacement leaves the
// last-known-good carrier available to its owner.
package networkrecovery

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/diagnostics"
	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/shared/supportref"
)

type Controller struct {
	config Config

	mu                sync.RWMutex
	pending           *Observation
	pendingAt         time.Time
	latestGeneration  uint64
	activeGeneration  uint64
	retryObservation  *Observation
	retryAt           time.Time
	attempt           uint32
	stableSince       time.Time
	health            HealthSnapshot
	wake              chan struct{}
	runCancel         context.CancelFunc
	runDone           chan struct{}
	sourceDone        chan struct{}
	incident          bool
	incidentCaptured  bool
	sourceFailed      bool
	shutdownErr       error
	started           bool
	stopped           bool
	debounceTimer     Timer
	retryTimer        Timer
	stableTimer       Timer
	workMu            sync.Mutex
	attemptCancel     context.CancelFunc
	attemptGeneration uint64
	attemptToken      uint64
}

func New(config Config) (*Controller, error) {
	if err := config.Identity.Validate(); err != nil || config.Replacer == nil {
		return nil, ErrInvalidConfiguration
	}
	if config.Clock == nil {
		config.Clock = realClock{}
	}
	if config.Debounce == 0 {
		config.Debounce = defaultDebounce
	}
	if config.InitialBackoff == 0 {
		config.InitialBackoff = defaultInitialBackoff
	}
	if config.MaxBackoff == 0 {
		config.MaxBackoff = defaultMaxBackoff
	}
	if config.StableReadyWindow == 0 {
		config.StableReadyWindow = defaultStableReadyWindow
	}
	if config.Debounce < 0 || config.InitialBackoff <= 0 || config.MaxBackoff < config.InitialBackoff || config.StableReadyWindow <= 0 {
		return nil, ErrInvalidConfiguration
	}
	if config.Jitter == nil {
		config.Jitter = func(capacity time.Duration) time.Duration {
			if capacity <= 0 {
				return 0
			}
			return time.Duration(rand.Float64() * float64(capacity))
		}
	}
	c := &Controller{config: config, wake: make(chan struct{}, 1)}
	c.health = HealthSnapshot{State: StateUnknown, Code: "network_waiting", CheckedAt: config.Clock.Now()}
	return c, nil
}

// Observe accepts a network observation. Events from an older generation are
// ignored; events in the same debounce window are merged by generation.
func (c *Controller) Observe(observation Observation) error {
	if c == nil {
		return ErrInvalidConfiguration
	}
	if err := observation.Validate(); err != nil {
		return err
	}
	if observation.At.IsZero() {
		observation.At = c.config.Clock.Now()
	}
	var cancelAttempt context.CancelFunc
	c.mu.Lock()
	if c.stopped {
		c.mu.Unlock()
		return ErrStopped
	}
	if observation.Generation < c.latestGeneration {
		c.mu.Unlock()
		return nil
	}
	isNewGeneration := observation.Generation > c.latestGeneration
	c.latestGeneration = observation.Generation
	if c.pending == nil || isNewGeneration || c.pending.Generation != observation.Generation {
		copyObservation := observation
		c.pending = &copyObservation
	} else {
		c.pending.Reasons |= observation.Reasons
		c.pending.Viable = observation.Viable
		c.pending.At = observation.At
	}
	c.pendingAt = observation.At
	if isNewGeneration {
		// A new network generation is a new state decision. It is allowed to
		// retry a previously permanent auth/config failure after fresh state.
		c.retryObservation = nil
		c.retryAt = time.Time{}
		c.attempt = 0
		c.stableSince = time.Time{}
		cancelAttempt = c.attemptCancel
	}
	c.health.State = StateDegraded
	c.health.Code = "network_change_pending"
	c.health.Failure = ""
	c.health.Reason = c.pending.Reasons
	c.health.NetworkGeneration = c.latestGeneration
	c.health.NextRetryAt = time.Time{}
	c.health.StableSince = time.Time{}
	c.health.CheckedAt = c.config.Clock.Now()
	c.mu.Unlock()
	if cancelAttempt != nil {
		cancelAttempt()
	}
	c.signal()
	return nil
}

// Flush forces the current debounced observation to be processed. It is useful
// for deterministic callers and is also a safe synchronous recovery primitive
// for a service that has its own event loop.
func (c *Controller) Flush(ctx context.Context) error {
	if ctx == nil {
		return ErrInvalidConfiguration
	}
	observation, ok := c.takePending(true)
	if !ok {
		observation, ok = c.takeRetry()
		if !ok {
			return ErrNoPendingChange
		}
	}
	return c.replace(ctx, observation, false)
}

// Tick processes work whose injected clock says it is due. It is primarily
// useful to deterministic service loops and tests; Start performs the same
// work automatically with timers.
func (c *Controller) Tick(ctx context.Context) error {
	if ctx == nil {
		return ErrInvalidConfiguration
	}
	if observation, ok := c.takePending(false); ok {
		return c.replace(ctx, observation, false)
	}
	if observation, ok := c.takeRetry(); ok {
		return c.replace(ctx, observation, false)
	}
	c.resetAfterStable()
	return nil
}

// Start starts the optional source and the controller's debounce/retry loop.
func (c *Controller) Start(ctx context.Context) error {
	if c == nil || ctx == nil {
		return ErrInvalidConfiguration
	}
	c.mu.Lock()
	if c.started || c.stopped {
		c.mu.Unlock()
		return ErrInvalidConfiguration
	}
	if supportref.FromContext(ctx) == "" {
		ctx = supportref.WithContext(ctx, supportref.New())
	}
	runCtx, cancel := context.WithCancel(ctx)
	c.started = true
	c.runCancel = cancel
	c.runDone = make(chan struct{})
	c.debounceTimer = c.config.Clock.NewTimer(time.Hour)
	c.retryTimer = c.config.Clock.NewTimer(time.Hour)
	c.stableTimer = c.config.Clock.NewTimer(time.Hour)
	c.debounceTimer.Stop()
	c.retryTimer.Stop()
	c.stableTimer.Stop()
	done := c.runDone
	source := c.config.Source
	if source != nil {
		c.sourceDone = make(chan struct{})
	}
	sourceDone := c.sourceDone
	c.mu.Unlock()
	if source != nil {
		events, err := source.Start(runCtx)
		if err != nil {
			cancel()
			cleanupErr := source.Close()
			close(sourceDone)
			close(done)
			c.mu.Lock()
			c.started = false
			c.runCancel = nil
			c.runDone = nil
			c.mu.Unlock()
			return Failure{Class: FailureUnknown, Cause: errors.Join(err, cleanupErr)}
		}
		go func() { defer close(sourceDone); c.consume(runCtx, events) }()
	}
	go c.loop(runCtx, done)
	return nil
}

func (c *Controller) consume(ctx context.Context, events <-chan Observation) {
	if events == nil {
		c.sourceFailure(ctx, ErrInvalidObservation, true)
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case observation, ok := <-events:
			if !ok {
				if ctx.Err() == nil {
					c.sourceFailure(ctx, ErrSourceUnavailable, true)
				}
				return
			}
			if err := c.Observe(observation); err != nil && err != ErrStopped {
				c.sourceFailure(ctx, err, false)
			}
		}
	}
}

// Stop cancels any in-flight replacement and closes the source. It is safe to
// call multiple times and preserves bounded shutdown under cancellation.
func (c *Controller) Stop(ctx context.Context) error {
	if c == nil {
		return nil
	}
	if ctx == nil {
		return ErrInvalidConfiguration
	}
	c.mu.Lock()
	if c.stopped {
		c.mu.Unlock()
		return nil
	}
	c.stopped = true
	c.health.State = StateStopped
	c.health.Code = "stopped"
	c.health.NextRetryAt = time.Time{}
	c.health.CheckedAt = c.config.Clock.Now()
	cancel, done, source, sourceDone := c.runCancel, c.runDone, c.config.Source, c.sourceDone
	c.runCancel = nil
	c.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	var sourceErr error
	if source != nil {
		sourceErr = source.Close()
	}
	for _, worker := range []chan struct{}{done, sourceDone} {
		if worker == nil {
			continue
		}
		select {
		case <-worker:
		case <-ctx.Done():
			return Failure{Class: FailureUnknown, Cause: errors.Join(sourceErr, ctx.Err())}
		}
	}
	c.mu.RLock()
	shutdownErr := c.shutdownErr
	c.mu.RUnlock()
	if cause := errors.Join(sourceErr, shutdownErr); cause != nil {
		return Failure{Class: FailureUnknown, Cause: cause}
	}
	return nil
}

func (c *Controller) Shutdown(ctx context.Context) error { return c.Stop(ctx) }

// Health returns a copy suitable for health endpoints and metrics adapters.
func (c *Controller) Health() HealthSnapshot {
	if c == nil {
		return HealthSnapshot{State: StateDown, Code: "invalid"}
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	snapshot := c.health
	snapshot.CheckedAt = c.config.Clock.Now()
	return snapshot
}

func (c *Controller) Snapshot() HealthSnapshot { return c.Health() }

func (c *Controller) NextRetryAt() (time.Time, bool) { return c.Health().NextRetry() }

func (c *Controller) signal() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

func (c *Controller) loop(ctx context.Context, done chan struct{}) {
	defer close(done)
	for {
		c.configureTimers()
		var debounceC, retryC, stableC <-chan time.Time
		c.mu.RLock()
		if c.debounceTimer != nil && c.pending != nil {
			debounceC = c.debounceTimer.C()
		}
		if c.retryTimer != nil && !c.retryAt.IsZero() {
			retryC = c.retryTimer.C()
		}
		if c.stableTimer != nil && !c.stableSince.IsZero() {
			stableC = c.stableTimer.C()
		}
		c.mu.RUnlock()
		select {
		case <-ctx.Done():
			return
		case <-c.wake:
			continue
		case <-debounceC:
			if observation, ok := c.takePending(false); ok {
				_ = c.replace(ctx, observation, true)
			}
		case <-retryC:
			if observation, ok := c.takeRetry(); ok {
				_ = c.replace(ctx, observation, true)
			}
		case <-stableC:
			c.resetAfterStable()
		}
	}
}

func (c *Controller) configureTimers() {
	now := c.config.Clock.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.debounceTimer != nil {
		if c.pending == nil {
			c.debounceTimer.Stop()
		} else {
			c.resetTimer(c.debounceTimer, remaining(c.pendingAt.Add(c.config.Debounce), now))
		}
	}
	if c.retryTimer != nil {
		if c.retryAt.IsZero() {
			c.retryTimer.Stop()
		} else {
			c.resetTimer(c.retryTimer, remaining(c.retryAt, now))
		}
	}
	if c.stableTimer != nil {
		if c.stableSince.IsZero() {
			c.stableTimer.Stop()
		} else {
			c.resetTimer(c.stableTimer, remaining(c.stableSince.Add(c.config.StableReadyWindow), now))
		}
	}
}

func (c *Controller) resetTimer(timer Timer, delay time.Duration) {
	if delay < 0 {
		delay = 0
	}
	if !timer.Stop() {
		select {
		case <-timer.C():
		default:
		}
	}
	timer.Reset(delay)
}

func remaining(deadline, now time.Time) time.Duration {
	if !deadline.After(now) {
		return 0
	}
	return deadline.Sub(now)
}

func (c *Controller) takePending(force bool) (Observation, bool) {
	now := c.config.Clock.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.pending == nil || (!force && now.Before(c.pendingAt.Add(c.config.Debounce))) {
		return Observation{}, false
	}
	observation := *c.pending
	c.pending = nil
	c.pendingAt = time.Time{}
	return observation, true
}

func (c *Controller) takeRetry() (Observation, bool) {
	now := c.config.Clock.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.retryObservation == nil || c.retryAt.IsZero() || now.Before(c.retryAt) || c.retryObservation.Generation != c.latestGeneration {
		return Observation{}, false
	}
	observation := *c.retryObservation
	c.retryObservation = nil
	c.retryAt = time.Time{}
	return observation, true
}

func (c *Controller) replace(ctx context.Context, observation Observation, automatic bool) error {
	c.workMu.Lock()
	defer c.workMu.Unlock()
	if err := observation.Validate(); err != nil {
		return err
	}
	c.mu.Lock()
	if c.stopped {
		c.mu.Unlock()
		return ErrStopped
	}
	if observation.Generation < c.latestGeneration {
		c.mu.Unlock()
		return nil
	}
	attempt := c.attempt + 1
	previous := c.activeGeneration
	identity := c.config.Identity
	c.health.State = StateDegraded
	c.health.Code = "carrier_replacing"
	c.health.Failure = ""
	c.health.Attempt = attempt
	c.health.Reason = observation.Reasons
	c.health.NetworkGeneration = observation.Generation
	c.health.NextRetryAt = time.Time{}
	c.health.StableSince = time.Time{}
	c.health.CheckedAt = c.config.Clock.Now()
	c.mu.Unlock()

	request := ReplacementRequest{Identity: identity, NetworkGeneration: observation.Generation, PreviousNetworkGeneration: previous, Reasons: observation.Reasons, Attempt: attempt}
	if err := request.Validate(); err != nil {
		return c.fail(ctx, observation, err, automatic)
	}
	if observation.Reasons&ReasonPathViability != 0 && !observation.Viable {
		c.markUnavailable(observation)
		return nil
	}
	attemptCtx, cancelAttempt := context.WithCancel(ctx)
	c.mu.Lock()
	if observation.Generation < c.latestGeneration || c.stopped {
		c.mu.Unlock()
		cancelAttempt()
		return nil
	}
	c.attemptCancel = cancelAttempt
	c.attemptGeneration = observation.Generation
	c.attemptToken++
	attemptToken := c.attemptToken
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		if c.attemptGeneration == observation.Generation && c.attemptToken == attemptToken {
			c.attemptCancel = nil
			c.attemptGeneration = 0
		}
		c.mu.Unlock()
		cancelAttempt()
	}()
	result, err := c.config.Replacer.Replace(attemptCtx, request)
	if err == nil {
		if resultErr := result.Validate(); resultErr != nil {
			err = resultErr
		} else if result.Identity != identity {
			err = ErrIdentityChanged
		} else if result.NetworkGeneration != observation.Generation {
			err = fmt.Errorf("%w: network generation %d", ErrInvalidObservation, result.NetworkGeneration)
		}
	}
	if err != nil {
		return c.fail(ctx, observation, err, automatic)
	}
	c.mu.Lock()

	if c.stopped {
		c.mu.Unlock()
		return ErrStopped
	}
	// A newer event may have arrived while the carrier was being staged. Never
	// let an older completion replace the newer network state.
	if observation.Generation < c.latestGeneration {
		c.mu.Unlock()
		return nil
	}
	c.activeGeneration = result.NetworkGeneration
	c.retryObservation = nil
	c.retryAt = time.Time{}
	c.stableSince = c.config.Clock.Now()
	c.health.State = StateReady
	c.health.Code = "carrier_ready"
	c.health.Failure = ""
	c.health.NetworkGeneration = c.latestGeneration
	c.health.ActiveNetworkGeneration = c.activeGeneration
	c.health.StableSince = c.stableSince
	c.health.NextRetryAt = time.Time{}
	c.health.CheckedAt = c.config.Clock.Now()
	recovered := c.incident && !c.sourceFailed
	if recovered {
		c.incident = false
		c.incidentCaptured = false
	}
	if c.sourceFailed {
		c.health.State = StateDegraded
		c.health.Code = "network_source_failed"
	}
	c.mu.Unlock()
	c.signal()
	if recovered {
		c.publishRecovery(ctx)
	}
	return nil
}

func (c *Controller) markUnavailable(observation Observation) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stopped || observation.Generation < c.latestGeneration {
		return
	}
	c.retryObservation = nil
	c.retryAt = time.Time{}
	c.stableSince = time.Time{}
	c.health.State = StateDown
	c.health.Code = "network_unavailable"
	c.health.Failure = ""
	c.health.Reason = observation.Reasons
	c.health.NetworkGeneration = observation.Generation
	c.health.ActiveNetworkGeneration = c.activeGeneration
	c.health.NextRetryAt = time.Time{}
	c.health.StableSince = time.Time{}
	c.health.CheckedAt = c.config.Clock.Now()
}

func (c *Controller) fail(ctx context.Context, observation Observation, err error, automatic bool) error {
	failure := Classify(err)
	if failure.Class == FailureCanceled {
		return err
	}
	c.mu.Lock()
	notify := false
	defer func() {
		c.mu.Unlock()
		if notify {
			c.publishFailure(ctx, err, failure.Permanent && failure.Class != FailureAuthentication)
		}
	}()
	if c.stopped {
		if automatic {
			c.shutdownErr = err
		}
		return Failure{Class: FailureUnknown, Cause: errors.Join(ErrStopped, err)}
	}
	if observation.Generation < c.latestGeneration {
		return err
	}
	capture := failure.Permanent && failure.Class != FailureAuthentication
	if automatic && (!c.incident || capture && !c.incidentCaptured) {
		c.incident = true
		if capture {
			c.incidentCaptured = true
		}
		notify = true
	}
	c.attempt++
	c.health.Failure = failure.Class
	c.health.Reason = observation.Reasons
	c.health.NetworkGeneration = c.latestGeneration
	c.health.ActiveNetworkGeneration = c.activeGeneration
	c.health.Attempt = c.attempt
	c.health.StableSince = time.Time{}
	c.health.CheckedAt = c.config.Clock.Now()
	if failure.Permanent {
		c.retryObservation = nil
		c.retryAt = time.Time{}
		c.health.State = StateDown
		c.health.Code = failureCode(failure.Class)
		c.health.NextRetryAt = time.Time{}
		return err
	}
	delay := c.backoff(c.attempt)
	if delay < 0 {
		delay = 0
	}
	c.retryAt = c.config.Clock.Now().Add(delay)
	copyObservation := observation
	c.retryObservation = &copyObservation
	c.health.State = StateDegraded
	c.health.Code = "carrier_retry_scheduled"
	c.health.NextRetryAt = c.retryAt
	c.signal()
	return err
}

func (c *Controller) backoff(attempt uint32) time.Duration {
	capDelay := c.config.InitialBackoff
	for i := uint32(1); i < attempt && capDelay < c.config.MaxBackoff; i++ {
		if capDelay > c.config.MaxBackoff/2 {
			capDelay = c.config.MaxBackoff
			break
		}
		capDelay *= 2
	}
	if capDelay > c.config.MaxBackoff {
		capDelay = c.config.MaxBackoff
	}
	delay := c.config.Jitter(capDelay)
	if delay < 0 {
		return 0
	}
	if delay > capDelay {
		return capDelay
	}
	return delay
}

func (c *Controller) resetAfterStable() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sourceFailed || c.stableSince.IsZero() || c.config.Clock.Now().Before(c.stableSince.Add(c.config.StableReadyWindow)) || c.activeGeneration == 0 || c.activeGeneration != c.latestGeneration {
		return
	}
	c.attempt = 0
	c.retryObservation = nil
	c.retryAt = time.Time{}
	c.health.State = StateReady
	c.health.Code = "carrier_stable"
	c.health.Failure = ""
	c.health.Attempt = 0
	c.health.NextRetryAt = time.Time{}
	c.health.CheckedAt = c.config.Clock.Now()
	c.stableSince = time.Time{}
	c.health.StableSince = time.Time{}
}

func (c *Controller) sourceFailure(ctx context.Context, err error, unavailable bool) {
	c.mu.Lock()
	if c.stopped || ctx.Err() != nil {
		c.mu.Unlock()
		return
	}
	notify := !c.incidentCaptured
	c.incidentCaptured = true
	c.incident = true
	c.sourceFailed = c.sourceFailed || unavailable
	c.health.State = StateDown
	c.health.Code = "network_source_failed"
	c.health.Failure = FailureLocalSystem
	c.mu.Unlock()
	if notify {
		c.publishFailure(ctx, err, true)
	}
}
func (c *Controller) publishFailure(ctx context.Context, err error, capture bool) {
	reporter := errorreport.Current()
	if capture {
		reporter.CaptureFailure(ctx, "paperboat-daemon", "tunnel_bootstrap", "reconciliation", "transport_failed", err)
	} else {
		reporter.ObserveFailure(ctx, "paperboat-daemon", "tunnel_bootstrap", "reconciliation", "transport_failed", err)
	}
}
func (c *Controller) publishRecovery(ctx context.Context) {
	if recorder := diagnostics.FromContext(ctx); recorder != nil {
		if err := recorder.RecordWithSupportReference("network", "carrier_recovered", "info", supportref.FromContext(ctx), map[string]string{"operation": "tunnel_bootstrap", "outcome": "success", "state": "ready"}); err != nil {
			errorreport.Current().ObserveFailure(ctx, "paperboat-daemon", "tunnel_bootstrap", "diagnostic_storage", "diagnostic_storage_unavailable", err)
		}
	}
	errorreport.Current().Lifecycle(ctx, "config", "tunnel_bootstrap", "recovered", "success")
}
