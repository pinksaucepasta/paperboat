// Package hostd owns Paperboat workloads which are required to outlive a
// replaceable runtime worker.  In particular, a worker is never allowed to
// acquire a session manager or a live PTY.
package hostd

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/diagnostics"
	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/execprocess"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/filetransfer"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/session"
	"github.com/pinksaucepasta/paperboat/internal/managedssh"
	"github.com/pinksaucepasta/paperboat/shared/backoff"
	"github.com/pinksaucepasta/paperboat/shared/supportref"
)

var (
	ErrInvalidConfig = errors.New("invalid hostd configuration")
	ErrInvalidState  = errors.New("invalid hostd state")
)

// ReplacementCommittedError reports cleanup failure after the candidate has
// already become the sole coordination worker. Callers must publish the new
// worker identity while still surfacing the predecessor cleanup error.
type ReplacementCommittedError struct{ Err error }

func (e *ReplacementCommittedError) Error() string {
	if e == nil || e.Err == nil {
		return "worker replacement committed"
	}
	return "worker replacement committed; the new worker is active, but previous worker cleanup failed. Run pb doctor to check runtime health"
}

func (*ReplacementCommittedError) DiagnosticStage() string { return "component_shutdown" }
func (*ReplacementCommittedError) DiagnosticCode() string  { return "shutdown_failed" }

func (e *ReplacementCommittedError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// Service is deliberately the small lifecycle contract shared by stable
// ingress/workload services and replaceable coordination services. Shutdown
// must be idempotent and clean partial allocations even when Start returned an
// error; hostd always invokes it for a failed start. Start must work again after a
// failed Start and that Shutdown: hostd retries a failed optional component.
type Service interface {
	Start(context.Context) error
	Shutdown(context.Context) error
}

type Component struct {
	Name     string
	Required bool
	Service  Service
}

// Workloads is the only owner of live workload managers.  It is intentionally
// exposed read-only through Daemon so protocol handlers can use the same
// managers without duplicating process ownership in workers.
type Workloads struct {
	Sessions   session.Service
	Executions execprocess.Service
	Transfers  *filetransfer.Service
	ManagedSSH *managedssh.Host
}

func (w Workloads) valid() bool {
	// Sessions and executions share terminal workload ownership. Transfers
	// remain durable across coordination-worker replacement.
	return w.Transfers != nil && (w.Sessions == nil) == (w.Executions == nil)
}

type Config struct {
	Workloads       Workloads
	Components      []Component
	ShutdownTimeout time.Duration
	// Report receives each optional component's failure (with the wait before its next attempt, or 0 when a
	// failed rollback stops the retries) and its recovery (nil error), so a skipped component is a failing
	// step, not a silent one.
	Report func(component string, err error, retryAfter time.Duration)
}

// The failed optional component retry schedule: doubling from 5 s to 5 min, jittered (shared/backoff).
var (
	componentRetryFirst   = 5 * time.Second
	componentRetryMaximum = 5 * time.Minute
)

// Daemon is the stable host supervisor.  Its Shutdown method is reserved for
// actual hostd shutdown.  Worker replacement is performed by WorkerController
// and never invokes Daemon.Shutdown.
type Daemon struct {
	mu          sync.RWMutex
	config      Config
	started     []Component
	running     bool
	stopped     bool
	failedStart bool
	recorder    *diagnostics.Recorder
	reference   string
	// retryCtx ends the optional component retries; retries waits for them.
	retryCtx    context.Context
	retryCancel context.CancelFunc
	retries     sync.WaitGroup
}

func New(config Config) (*Daemon, error) {
	if !config.Workloads.valid() || len(config.Components) == 0 {
		return nil, ErrInvalidConfig
	}
	if config.ShutdownTimeout == 0 {
		config.ShutdownTimeout = 30 * time.Second
	}
	if config.ShutdownTimeout <= 0 {
		return nil, ErrInvalidConfig
	}
	seen := make(map[string]struct{}, len(config.Components))
	for _, component := range config.Components {
		if component.Name == "" || component.Service == nil {
			return nil, ErrInvalidConfig
		}
		if _, exists := seen[component.Name]; exists {
			return nil, ErrInvalidConfig
		}
		seen[component.Name] = struct{}{}
	}
	return &Daemon{config: config}, nil
}

func (d *Daemon) Workloads() Workloads { return d.config.Workloads }

func (d *Daemon) Start(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.running || d.stopped || len(d.started) > 0 {
		return ErrInvalidState
	}
	d.recorder = diagnostics.FromContext(ctx)
	d.reference = supportref.FromContext(ctx)
	if d.reference == "" {
		d.reference = supportref.New()
	}
	ctx = d.diagnosticContext(ctx)
	for _, component := range d.config.Components {
		if err := invokeComponent(ctx, component, "component_start", component.Service.Start); err != nil {
			cleanupCtx, cancel := detachedShutdownContext(ctx, d.config.ShutdownTimeout)
			failedCleanupErr := invokeComponent(cleanupCtx, component, "component_rollback", component.Service.Shutdown)
			cancel()
			if !component.Required && failedCleanupErr == nil {
				d.optionalFailedLocked(ctx, component, err)
				continue
			}
			if failedCleanupErr != nil {
				d.started = append(d.started, component)
			}
			cleanupCtx, cancel = detachedShutdownContext(ctx, d.config.ShutdownTimeout)
			cleanupErr := d.shutdownStarted(cleanupCtx, "component_rollback")
			cancel()
			d.failedStart = true
			return errors.Join(fmt.Errorf("start stable %s: %w", component.Name, err), failedCleanupErr, cleanupErr)
		}
		d.started = append(d.started, component)
	}
	d.running = true
	d.failedStart = false
	return nil
}

func (d *Daemon) Shutdown(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	d.stopRetries()
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.stopped {
		return nil
	}
	if !d.running && !d.failedStart && len(d.started) == 0 {
		return ErrInvalidState
	}
	ctx = d.diagnosticContext(ctx)
	shutdownCtx, cancel := context.WithTimeout(ctx, d.config.ShutdownTimeout)
	defer cancel()
	err := d.shutdownStarted(shutdownCtx, "component_shutdown")
	d.running = false
	if d.failedStart {
		if len(d.started) == 0 {
			d.failedStart = false
		}
		return err
	}
	if len(d.started) == 0 {
		d.stopped = true
	}
	return err
}

// optionalFailedLocked retries a skipped optional component in the background, reporting it until it starts.
// Called with d.mu held.
func (d *Daemon) optionalFailedLocked(ctx context.Context, component Component, err error) {
	if d.retryCtx == nil {
		d.retryCtx, d.retryCancel = context.WithCancel(context.WithoutCancel(ctx))
	}
	retryCtx := d.retryCtx
	d.retries.Add(1)
	go func() {
		defer d.retries.Done()
		d.retryComponent(retryCtx, component, err)
	}()
}

// retryComponent starts component again after backoff.Retry waits until it starts or ctx ends. A started
// component joins d.started, so Shutdown stops it like any other.
func (d *Daemon) retryComponent(ctx context.Context, component Component, cause error) {
	for failures := 1; ; failures++ {
		wait := backoff.Retry(failures, componentRetryFirst, componentRetryMaximum)
		d.report(component.Name, cause, wait)
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		cause = invokeComponent(ctx, component, "component_start", component.Service.Start)
		if cause == nil {
			d.mu.Lock()
			stopping := ctx.Err() != nil
			if !stopping {
				d.started = append(d.started, component)
			}
			d.mu.Unlock()
			if stopping {
				cleanupCtx, cancel := detachedShutdownContext(ctx, d.config.ShutdownTimeout)
				_ = invokeComponent(cleanupCtx, component, "component_shutdown", component.Service.Shutdown)
				cancel()
				return
			}
			d.report(component.Name, nil, 0)
			return
		}
		cleanupCtx, cancel := detachedShutdownContext(ctx, d.config.ShutdownTimeout)
		cleanupErr := invokeComponent(cleanupCtx, component, "component_rollback", component.Service.Shutdown)
		cancel()
		if cleanupErr != nil {
			// Partial allocations remain: another Start could duplicate them. Shutdown retries the cleanup.
			d.mu.Lock()
			d.started = append(d.started, component)
			d.mu.Unlock()
			d.report(component.Name, errors.Join(cause, cleanupErr), 0)
			return
		}
	}
}

// stopRetries ends the optional component retries and waits for them, before Shutdown takes d.mu (a retry
// that just started its component takes d.mu to join d.started).
func (d *Daemon) stopRetries() {
	d.mu.Lock()
	cancel := d.retryCancel
	d.retryCtx, d.retryCancel = nil, nil
	d.mu.Unlock()
	if cancel != nil {
		cancel()
		d.retries.Wait()
	}
}

func (d *Daemon) report(name string, err error, retryAfter time.Duration) {
	if d.config.Report == nil {
		return
	}
	// invokeComponent's wrapper only says a lifecycle step failed; the report names the cause.
	var owned reportedComponentError
	if errors.As(err, &owned) && owned.cause != nil {
		err = owned.cause
	}
	d.config.Report(name, err, retryAfter)
}

func detachedShutdownContext(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithTimeout(context.WithoutCancel(ctx), timeout)
}

// invokeComponent's lifecycle reporter owns this exact component failure.
type reportedComponentError struct{ cause error }

func (e reportedComponentError) Error() string                      { return "Host component lifecycle failed." }
func (e reportedComponentError) Unwrap() error                      { return e.cause }
func (e reportedComponentError) RuntimeLifecycleFailureOwned() bool { return true }

// The process owns the recorder; supervisor shutdown only borrows it.
func (d *Daemon) diagnosticContext(ctx context.Context) context.Context {
	if supportref.FromContext(ctx) == "" {
		ctx = supportref.WithContext(ctx, d.reference)
	}
	if diagnostics.FromContext(ctx) == nil && d.recorder != nil {
		ctx = diagnostics.WithRecorder(ctx, d.recorder)
	}
	return ctx
}

func (d *Daemon) shutdownStarted(ctx context.Context, stage string) error {
	var result error
	failed := make([]Component, 0, len(d.started))
	for index := len(d.started) - 1; index >= 0; index-- {
		component := d.started[index]
		if err := invokeComponent(ctx, component, stage, component.Service.Shutdown); err != nil {
			result = errors.Join(result, err)
			failed = append(failed, component)
		}
	}
	for left, right := 0, len(failed)-1; left < right; left, right = left+1, right-1 {
		failed[left], failed[right] = failed[right], failed[left]
	}
	d.started = failed
	return result
}

func (d *Daemon) Running() bool {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.running && !d.stopped
}

// WorkerController changes coordination workers while retaining daemon-owned
// workloads.  Candidate start happens before the prior worker is stopped so a
// failed candidate cannot interrupt existing coordination.
type WorkerController struct {
	mu      sync.Mutex
	daemon  *Daemon
	active  Service
	pending []Service
	running bool
}

type workerLifecycleFailure struct {
	startErr   error
	cleanupErr error
}

func (e *workerLifecycleFailure) Error() string {
	if e == nil {
		return "coordination worker lifecycle failed."
	}
	if e.startErr != nil && e.cleanupErr != nil {
		return "coordination worker start failed and partial cleanup remains; retry worker shutdown before restarting."
	}
	if e.startErr != nil {
		return "coordination worker start failed; partial cleanup completed."
	}
	return "coordination worker shutdown failed; retry shutdown to finish cleanup."
}

func (e *workerLifecycleFailure) Unwrap() []error {
	if e == nil {
		return nil
	}
	causes := make([]error, 0, 2)
	if e.startErr != nil {
		causes = append(causes, e.startErr)
	}
	if e.cleanupErr != nil {
		causes = append(causes, e.cleanupErr)
	}
	return causes
}

func NewWorkerController(daemon *Daemon) (*WorkerController, error) {
	if daemon == nil {
		return nil, ErrInvalidConfig
	}
	return &WorkerController{daemon: daemon}, nil
}

func (c *WorkerController) Start(ctx context.Context, worker Service) error {
	if worker == nil {
		return ErrInvalidState
	}
	if ctx == nil {
		ctx = context.Background()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.daemon.mu.RLock()
	defer c.daemon.mu.RUnlock()
	if !c.daemon.running || c.daemon.stopped {
		return ErrInvalidState
	}
	if c.running || c.active != nil || len(c.pending) > 0 {
		return ErrInvalidState
	}
	ctx = c.daemon.diagnosticContext(ctx)
	startErr := worker.Start(ctx)
	if startErr == nil {
		startErr = ctx.Err()
	}
	if startErr != nil {
		cleanupCtx, cancel := detachedShutdownContext(ctx, c.daemon.config.ShutdownTimeout)
		cleanupErr := worker.Shutdown(cleanupCtx)
		if cleanupErr == nil {
			cleanupErr = cleanupCtx.Err()
		}
		cancel()
		if cleanupErr != nil {
			c.pending = append(c.pending, worker)
		}
		return &workerLifecycleFailure{startErr: startErr, cleanupErr: cleanupErr}
	}
	c.active, c.running = worker, true
	return nil
}

// Replace starts a ready candidate, then stops the prior coordination worker.
// It never calls into daemon workloads and therefore cannot terminate a PTY,
// transfer, preview, serve listener, or managed SSH stream.
func (c *WorkerController) Replace(ctx context.Context, candidate Service) error {
	if candidate == nil {
		return ErrInvalidState
	}
	if ctx == nil {
		ctx = context.Background()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.daemon.mu.RLock()
	defer c.daemon.mu.RUnlock()
	if !c.daemon.running || c.daemon.stopped {
		return ErrInvalidState
	}
	if !c.running || c.active == nil || len(c.pending) > 0 {
		return ErrInvalidState
	}
	ctx = c.daemon.diagnosticContext(ctx)
	startErr := candidate.Start(ctx)
	if startErr == nil {
		startErr = ctx.Err()
	}
	if startErr != nil {
		cleanupCtx, cancel := detachedShutdownContext(ctx, c.daemon.config.ShutdownTimeout)
		cleanupErr := candidate.Shutdown(cleanupCtx)
		if cleanupErr == nil {
			cleanupErr = cleanupCtx.Err()
		}
		cancel()
		if cleanupErr != nil {
			c.pending = append(c.pending, candidate)
		}
		return &workerLifecycleFailure{startErr: startErr, cleanupErr: cleanupErr}
	}
	previous := c.active
	c.active = candidate
	stopErr := previous.Shutdown(ctx)
	if stopErr == nil {
		stopErr = ctx.Err()
	}
	if stopErr != nil {
		// The candidate is the controller's selected worker. Keep the predecessor
		// as a cleanup owner rather than re-activating a partially stopped service.
		c.pending = append(c.pending, previous)
		return &ReplacementCommittedError{Err: fmt.Errorf("stop fenced worker: %w", stopErr)}
	}
	return nil
}

func (c *WorkerController) Shutdown(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.daemon.mu.RLock()
	ctx = c.daemon.diagnosticContext(ctx)
	c.daemon.mu.RUnlock()
	var result error
	if c.active != nil {
		err := c.active.Shutdown(ctx)
		if err == nil {
			err = ctx.Err()
		}
		if err == nil {
			c.active = nil
		} else {
			result = errors.Join(result, err)
		}
	}
	failed := make([]Service, 0, len(c.pending))
	for index := len(c.pending) - 1; index >= 0; index-- {
		worker := c.pending[index]
		err := worker.Shutdown(ctx)
		if err == nil {
			err = ctx.Err()
		}
		if err != nil {
			result = errors.Join(result, err)
			failed = append(failed, worker)
		}
	}
	for left, right := 0, len(failed)-1; left < right; left, right = left+1, right-1 {
		failed[left], failed[right] = failed[right], failed[left]
	}
	c.pending = failed
	c.running = false
	if result != nil {
		return &workerLifecycleFailure{cleanupErr: result}
	}
	if c.active != nil || len(c.pending) > 0 {
		return &workerLifecycleFailure{cleanupErr: ErrInvalidState}
	}
	return nil
}

func invokeComponent(ctx context.Context, component Component, stage string, run func(context.Context) error) error {
	err := run(ctx)
	if err == nil {
		err = ctx.Err()
	}
	errorreport.Current().ServiceLifecycle(ctx, component.Name, stage, err)
	if err == nil {
		if recorder := diagnostics.FromContext(ctx); recorder != nil {
			code := "ready"
			if stage != "component_start" {
				code = "stopped"
			}
			_ = recorder.RecordWithSupportReference(stage, code, "info", supportref.FromContext(ctx), map[string]string{
				"component": "paperboat-daemon", "service_component": errorreport.ServiceComponent(component.Name),
				"operation": stage, "outcome": "success",
			})
		}
	}
	if err != nil {
		return reportedComponentError{cause: err}
	}
	return nil
}
