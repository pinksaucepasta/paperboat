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
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/preview"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/session"
	"github.com/pinksaucepasta/paperboat/internal/managedssh"
	"github.com/pinksaucepasta/paperboat/internal/supportref"
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
// error; hostd always invokes it for a failed start.
type Service interface {
	Start(context.Context) error
	Shutdown(context.Context) error
}

type Component struct {
	Name     string
	Required bool
	Service  Service
}

// TunnelWorkloads is the stable ownership boundary for durable tunnel
// reconciliation. Coordination-worker replacement may inspect counts but must
// never acquire or stop the manager.
type TunnelWorkloads interface {
	Service
	ResourceCounts() map[string]uint64
}

// Workloads is the only owner of live workload managers.  It is intentionally
// exposed read-only through Daemon so protocol handlers can use the same
// managers without duplicating process ownership in workers.
type Workloads struct {
	Sessions   session.Service
	Executions execprocess.Service
	Transfers  *filetransfer.Service
	Previews   *preview.Registry
	ManagedSSH *managedssh.Host
	Tunnels    TunnelWorkloads
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
}

// Daemon is the stable host supervisor.  Its Shutdown method is reserved for
// actual hostd shutdown.  Worker replacement is performed by WorkerController
// and never invokes Daemon.Shutdown.
type Daemon struct {
	mu        sync.RWMutex
	config    Config
	started   []Component
	running   bool
	stopped   bool
	recorder  *diagnostics.Recorder
	reference string
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
	if d.running || d.stopped {
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
			cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), d.config.ShutdownTimeout)
			failedCleanupErr := invokeComponent(cleanupCtx, component, "component_rollback", component.Service.Shutdown)
			cancel()
			if !component.Required && failedCleanupErr == nil {
				continue
			}
			cleanupCtx, cancel = context.WithTimeout(context.WithoutCancel(ctx), d.config.ShutdownTimeout)
			cleanupErr := d.shutdownStarted(cleanupCtx, "component_rollback")
			cancel()
			return errors.Join(fmt.Errorf("start stable %s: %w", component.Name, err), failedCleanupErr, cleanupErr)
		}
		d.started = append(d.started, component)
	}
	d.running = true
	return nil
}

func (d *Daemon) Shutdown(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.stopped {
		return nil
	}
	if !d.running {
		return ErrInvalidState
	}
	ctx = d.diagnosticContext(ctx)
	shutdownCtx, cancel := context.WithTimeout(ctx, d.config.ShutdownTimeout)
	defer cancel()
	err := d.shutdownStarted(shutdownCtx, "component_shutdown")
	d.running, d.stopped = false, true
	return err
}

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
	for index := len(d.started) - 1; index >= 0; index-- {
		component := d.started[index]
		result = errors.Join(result, invokeComponent(ctx, component, stage, component.Service.Shutdown))
	}
	d.started = nil
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
	running bool
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
	c.mu.Lock()
	defer c.mu.Unlock()
	c.daemon.mu.RLock()
	defer c.daemon.mu.RUnlock()
	if !c.daemon.running || c.daemon.stopped {
		return ErrInvalidState
	}
	if c.running {
		return ErrInvalidState
	}
	if err := worker.Start(ctx); err != nil {
		return err
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
	c.mu.Lock()
	defer c.mu.Unlock()
	c.daemon.mu.RLock()
	defer c.daemon.mu.RUnlock()
	if !c.daemon.running || c.daemon.stopped {
		return ErrInvalidState
	}
	if !c.running || c.active == nil {
		return ErrInvalidState
	}
	if err := candidate.Start(ctx); err != nil {
		return err
	}
	previous := c.active
	c.active = candidate
	if err := previous.Shutdown(ctx); err != nil {
		// The candidate is now the sole worker; do not re-activate a potentially
		// partially shut-down predecessor.
		return &ReplacementCommittedError{Err: fmt.Errorf("stop fenced worker: %w", err)}
	}
	return nil
}

func (c *WorkerController) Shutdown(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.running {
		return nil
	}
	c.running = false
	worker := c.active
	c.active = nil
	return worker.Shutdown(ctx)
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
	return err
}
