package tunnelmanager

import (
	"context"
	"errors"
	"hash/fnv"
	"io"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/connectorprotocol"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/connector"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/connectorrotation"
)

// Start starts the one serialized durable reconciliation loop and, when a
// control stream provider is configured, acquires the first authenticated
// bootstrap stream in the control lifecycle. Initial acquisition is retried
// with the same bounded backoff as reconnects, so a transient WSS outage does
// not turn a durable activation into a terminal local failure. The stream is
// then owned by a bounded control goroutine, so hostd never needs an out-of-
// band RunControl call.
func (a *ProductionAssembly) Start(ctx context.Context) error {
	if a == nil || a.Manager == nil || ctx == nil {
		return ErrProductionAssemblyInvalid
	}
	// A descriptor source bound to Welcome cannot prepare a carrier until the
	// control stream has delivered that frame. Start the manager lifecycle first
	// without its synchronous initial reconcile, then acquire control; the
	// manager's wake-up below reconciles once Welcome and the authoritative
	// snapshot are available. Assemblies without a control stream retain the
	// ordinary synchronous crash-recovery contract.
	startManager := a.Manager.Start
	if a.controlStream != nil {
		startManager = a.Manager.StartDeferred
	}
	if err := startManager(ctx); err != nil {
		return err
	}
	if a.networkRecovery != nil {
		if err := a.networkRecovery.Start(ctx); err != nil {
			return errors.Join(err, a.Manager.Shutdown(context.Background()))
		}
	}
	if a.controlStream == nil {
		return nil
	}
	controlCtx, done, err := a.claimControl(ctx)
	if err != nil {
		return err
	}
	// StartDeferred intentionally skips inline reconciliation. The initial
	// control worker acquires and authenticates WSS before waking the manager;
	// this keeps the Welcome-bound carrier source closed until a real stream
	// exists and lets activation remain pending while the edge recovers.
	go a.runInitialControlLoop(controlCtx, done)
	return nil
}

// runInitialControlLoop owns the first control stream attempt. It runs after
// Start returns so lifecycle startup never waits on the control endpoint. A
// successful stream is the readiness boundary: only then may the control loop
// start and the manager reconcile the Welcome-bound carrier source.
func (a *ProductionAssembly) runInitialControlLoop(ctx context.Context, done chan struct{}) {
	stream, err := a.acquireInitialControl(ctx)
	if err != nil {
		a.finishControl(done, err)
		return
	}
	go a.runControlLoop(ctx, done, a.Control, a.controlRunner, stream, a.helloRequestID)
	a.Manager.Notify()
}

func (a *ProductionAssembly) acquireInitialControl(ctx context.Context) (io.ReadWriteCloser, error) {
	if a == nil || ctx == nil || a.controlStream == nil {
		return nil, ErrProductionControlMissing
	}
	for attempt := 1; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		stream, err := a.controlStream(ctx)
		if err == nil && stream != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				_ = stream.Close()
				return nil, ctxErr
			}
			return stream, nil
		}
		if stream != nil {
			_ = stream.Close()
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		if err == nil {
			err = ErrProductionControlMissing
		}
		if errors.Is(err, ErrConnectorGone) {
			a.markConnectorGone()
			return nil, err
		}
		failure := errors.Join(ErrProductionControlMissing, err)
		a.reportControlFailure(failure, attempt)
		if err := waitProductionControlReconnect(ctx, productionControlReconnectDelay(attempt, a.connectorID)); err != nil {
			return nil, err
		}
	}
}

func waitProductionControlReconnect(ctx context.Context, delay time.Duration) error {
	if ctx == nil {
		return ErrProductionAssemblyInvalid
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// RunControl is the explicit test/embedding hook for one already-authenticated
// connector-v1 bootstrap stream. Production callers should use Start with a
// ControlStream provider, which owns acquisition and reconnects.
func (a *ProductionAssembly) RunControl(ctx context.Context, stream io.ReadWriteCloser, helloRequestID string) error {
	if a == nil || a.controlRunner == nil || ctx == nil {
		return ErrProductionAssemblyInvalid
	}
	if helloRequestID == "" {
		helloRequestID = a.helloRequestID
	}
	if helloRequestID == "" {
		return errors.Join(ErrProductionControlMissing, ErrProductionAssemblyInvalid)
	}
	controlCtx, done, err := a.claimControl(ctx)
	if err != nil {
		if stream != nil {
			_ = stream.Close()
		}
		return err
	}
	if stream == nil {
		if a.controlStream == nil {
			err = ErrProductionControlMissing
		} else {
			stream, err = a.controlStream(controlCtx)
		}
		if err != nil || stream == nil {
			if stream != nil {
				_ = stream.Close()
			}
			if err == nil {
				err = ErrProductionControlMissing
			}
			finalErr := errors.Join(ErrProductionControlMissing, err)
			a.finishControl(done, finalErr)
			return finalErr
		}
	}
	return a.runControlLoop(controlCtx, done, a.Control, a.controlRunner, stream, helloRequestID)
}

func (a *ProductionAssembly) claimControl(parent context.Context) (context.Context, chan struct{}, error) {
	if a == nil || parent == nil {
		return nil, nil, ErrProductionAssemblyInvalid
	}
	a.controlMu.Lock()
	defer a.controlMu.Unlock()
	if a.controlStarted {
		return nil, nil, ErrProductionControlStarted
	}
	controlCtx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	a.controlStarted = true
	a.controlCancel = cancel
	a.controlDone = done
	a.controlErr = nil
	return controlCtx, done, nil
}

func (a *ProductionAssembly) finishControl(done chan struct{}, err error) {
	if a == nil || done == nil {
		return
	}
	a.controlMu.Lock()
	if a.controlDone == done {
		a.controlErr = err
		close(done)
	}
	a.controlMu.Unlock()
}

func (a *ProductionAssembly) runControlLoop(ctx context.Context, done chan struct{}, control *connectorrotation.ControlSession, runner *connector.DataCarrierControlRunner, stream io.ReadWriteCloser, helloRequestID string) error {
	if a == nil || ctx == nil || done == nil || control == nil || runner == nil || stream == nil || helloRequestID == "" {
		err := ErrProductionAssemblyInvalid
		if stream != nil {
			_ = stream.Close()
		}
		a.finishControl(done, err)
		return err
	}
	if err := a.runControlAttempt(ctx, runner, stream, helloRequestID); err == nil && ctx.Err() == nil {
		return a.reconnectControl(ctx, done, ErrProductionControlDisconnected)
	} else if ctx.Err() != nil {
		resultErr := controlTerminationFailure{cause: errors.Join(err, ctx.Err(), context.Cause(ctx))}
		a.finishControl(done, resultErr)
		return resultErr
	} else {
		return a.reconnectControl(ctx, done, err)
	}
}

type controlTerminationFailure struct{ cause error }

func (controlTerminationFailure) Error() string {
	return "Tunnel control stopped; stream cleanup may not have finished."
}

func (e controlTerminationFailure) Unwrap() error { return e.cause }

// Only the exact session whose bootstrap was rejected may retire its stream.
func (a *ProductionAssembly) reconnectStaleSession(session string) {
	a.controlMu.Lock()
	stream := a.activeControl
	if a.activeSession != session {
		stream = nil
	}
	a.controlMu.Unlock()
	if stream != nil {
		_ = stream.Close()
	}
}

func (a *ProductionAssembly) runControlAttempt(ctx context.Context, runner *connector.DataCarrierControlRunner, stream io.ReadWriteCloser, helloRequestID string) error {
	a.controlMu.Lock()
	a.activeControl, a.activeSession = stream, ""
	a.controlMu.Unlock()
	defer func() { a.controlMu.Lock(); a.activeControl, a.activeSession = nil, ""; a.controlMu.Unlock() }()
	if a.observeWelcome != nil {
		stream = &welcomeObservingControlStream{ReadWriteCloser: stream, onWelcome: func(w connectorprotocol.Welcome) error {
			a.controlMu.Lock()
			a.activeSession = w.SessionID
			a.controlMu.Unlock()
			return a.observeWelcome(w)
		}}
	}
	err := runner.Run(ctx, stream, helloRequestID)
	if err == nil && ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

func (a *ProductionAssembly) reconnectControl(ctx context.Context, done chan struct{}, cause error) error {
	if cause == nil {
		cause = ErrProductionControlDisconnected
	}
	if ctx.Err() != nil {
		a.finishControl(done, ctx.Err())
		return ctx.Err()
	}
	a.reportControlFailure(cause, 1)
	if a.controlFactory == nil || a.controlStream == nil {
		finalErr := errors.Join(ErrProductionControlRestartRequired, cause)
		a.finishControl(done, finalErr)
		return finalErr
	}
	for attempt := 1; ; attempt++ {
		delay := productionControlReconnectDelay(attempt, a.connectorID)
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			a.finishControl(done, ctx.Err())
			return ctx.Err()
		case <-timer.C:
		}
		controlConfig, err := a.controlFactory(ctx, a.Applier)
		if err != nil {
			if errors.Is(err, ErrProductionControlRestartRequired) {
				finalErr := errors.Join(ErrProductionControlRestartRequired, err)
				a.finishControl(done, finalErr)
				return finalErr
			}
			a.reportControlFailure(err, attempt+1)
			continue
		}
		control, runner, err := a.newControl(controlConfig)
		if err != nil {
			if errors.Is(err, ErrProductionControlRestartRequired) {
				finalErr := errors.Join(ErrProductionControlRestartRequired, err)
				a.finishControl(done, finalErr)
				return finalErr
			}
			a.reportControlFailure(err, attempt+1)
			continue
		}
		// A reconnect's carrier descriptor is checked against the current
		// control Hello. Refresh this expectation before opening the new stream
		// so a later route reconciliation cannot compare the new carrier
		// process epoch with the initial bootstrap epoch.
		if a.deferredCarrier != nil {
			if err := a.deferredCarrier.UpdateHello(controlConfig.Hello); err != nil {
				finalErr := errors.Join(ErrProductionControlRestartRequired, err)
				a.finishControl(done, finalErr)
				return finalErr
			}
		}
		stream, err := a.controlStream(ctx)
		if err != nil || stream == nil {
			if stream != nil {
				_ = stream.Close()
			}
			if err == nil {
				err = ErrProductionControlMissing
			}
			if errors.Is(err, ErrConnectorGone) {
				a.markConnectorGone()
				a.finishControl(done, err)
				return err
			}
			a.reportControlFailure(errors.Join(ErrProductionControlMissing, err), attempt+1)
			continue
		}
		a.controlMu.Lock()
		a.Control = control
		a.controlRunner = runner
		a.controlMu.Unlock()
		connected := time.Now()
		if err := a.runControlAttempt(ctx, runner, stream, a.helloRequestID); err == nil && ctx.Err() == nil {
			cause = ErrProductionControlDisconnected
		} else if ctx.Err() != nil {
			a.finishControl(done, ctx.Err())
			return ctx.Err()
		} else {
			cause = err
		}
		if time.Since(connected) >= productionControlHealthyAfter {
			// A connection that stayed up is not part of the failure streak: start the backoff over.
			attempt = 0
		}
		a.reportControlFailure(cause, attempt+1)
	}
}

func (a *ProductionAssembly) newControl(config connectorrotation.ControlSessionConfig) (*connectorrotation.ControlSession, *connector.DataCarrierControlRunner, error) {
	if a == nil || a.Applier == nil {
		return nil, nil, ErrProductionAssemblyInvalid
	}
	if config.Hello.AccountID != a.accountID || config.Hello.TunnelID != a.tunnelID || config.Hello.ConnectorID != a.connectorID || config.Hello.HostID != a.hostID {
		return nil, nil, connectorprotocol.ErrIdentityMismatch
	}
	// The server persists the highest process generation ever accepted for a
	// connector and rejects a later session that reuses it. A reconnect must
	// therefore be a fresh authenticated process epoch, not merely a new
	// websocket/session ID. Keep the durable connector identity unchanged, but
	// fail closed when the injected factory hands us a stale Hello. The factory
	// owns the process-generation claim and must also re-sign Auth for it.
	a.controlMu.Lock()
	current := a.Control
	a.controlMu.Unlock()
	if current == nil || current.Session() == nil {
		return nil, nil, ErrProductionControlRestartRequired
	}
	currentGeneration := current.Session().Hello().ProcessGeneration
	if config.Hello.ProcessGeneration <= currentGeneration || config.Hello.Auth.ProcessGeneration != config.Hello.ProcessGeneration {
		return nil, nil, errors.Join(ErrProductionControlRestartRequired, ErrGenerationConflict)
	}
	config.Applier = a.Applier
	config.SnapshotReadiness = a.Applier
	control, err := connectorrotation.NewControlSession(config)
	if err != nil {
		return nil, nil, err
	}
	runner, err := connector.NewDataCarrierControlRunner(connector.DataCarrierControlRunnerConfig{Control: control})
	if err != nil {
		return nil, nil, err
	}
	return control, runner, nil
}

func (a *ProductionAssembly) reportControlFailure(err error, nextAttempt int) {
	if a == nil || a.Manager == nil || err == nil {
		return
	}
	var tunnelID, connectorID string
	a.controlMu.Lock()
	control := a.Control
	a.controlMu.Unlock()
	if control != nil && control.Session() != nil {
		hello := control.Session().Hello()
		tunnelID, connectorID = hello.TunnelID, hello.ConnectorID
	}
	now := a.Manager.now()
	a.Manager.config.Report(Observation{TunnelID: tunnelID, ConnectorID: connectorID, Code: CodeControlUnavailable, Retryable: true, NextRetryAt: now.Add(productionControlReconnectDelay(nextAttempt, connectorID)), ObservedAt: now, Err: err})
}

// productionControlHealthyAfter is how long a control connection must stay up before a later
// disconnect starts the reconnect backoff over.
const productionControlHealthyAfter = 30 * time.Second

// productionControlMaxReconnectDelay caps the reconnect backoff: a connector the control plane keeps
// refusing costs it about one request a minute instead of several a second. The doubling stops at
// two thirds of the cap so the per-connector spread (up to half again) still fits under it.
const productionControlMaxReconnectDelay = time.Minute

func productionControlReconnectDelay(attempt int, identity string) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	base := productionControlMaxReconnectDelay * 2 / 3
	if attempt <= 10 {
		base = min(100*time.Millisecond*time.Duration(1<<(attempt-1)), base)
	}
	// Derive a stable per-connector spread so independent connectors do not all
	// reconnect on the same tick while lifecycle tests remain reproducible.
	hash := fnv.New32a()
	_, _ = hash.Write([]byte(identity))
	spreadPercent := (hash.Sum32() + uint32(attempt*37)) % 50
	return base + time.Duration(spreadPercent)*base/100
}
