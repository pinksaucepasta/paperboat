package runtime

import (
	"context"
	"errors"
	"fmt"
	typedhealth "github.com/pinksaucepasta/paperboat/shared/health"
	sharedtelemetry "github.com/pinksaucepasta/paperboat/shared/telemetry"
	"strings"
	"sync"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/diagnostics"
	"github.com/pinksaucepasta/paperboat/cli/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/health"
	hosttelemetry "github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/telemetry"
	"github.com/pinksaucepasta/paperboat/shared/supportref"
)

var (
	ErrInvalidConfiguration = errors.New("invalid runtime configuration")
	ErrInvalidState         = errors.New("invalid runtime state")
)

type Service interface {
	Start(context.Context) error
	Shutdown(context.Context) error
}
type Component struct {
	Capability string
	Required   bool
	Service    Service
}
type Config struct {
	Version         string
	Components      []Component
	ShutdownTimeout time.Duration
	Clock           health.Clock

	// HealthTracker, Metrics, and EventLog are optional integration points for
	// the stable host telemetry contract. The legacy health registry remains
	// authoritative for existing callers, and these hooks are never allocated
	// implicitly so a nil configuration preserves the old behavior.
	HealthTracker *typedhealth.HealthTracker
	Metrics       *hosttelemetry.Registry
	EventLog      *sharedtelemetry.EventLog
	CorrelationID string
}
type State string

const (
	New      State = "new"
	Starting State = "starting"
	Running  State = "running"
	Stopping State = "stopping"
	Stopped  State = "stopped"
	Failed   State = "failed"
)

type Runtime struct {
	opMu        sync.Mutex
	mu          sync.RWMutex
	config      Config
	state       State
	started     []Component
	health      *health.Registry
	typedHealth *typedhealth.HealthTracker
	metrics     *hosttelemetry.Registry
	eventLog    *sharedtelemetry.EventLog
	correlation string
	// typedMu guards typedComponents and typedSequence: stable host component retries report from their own
	// goroutines.
	typedMu         sync.Mutex
	typedComponents map[typedhealth.Dimension]map[string]componentHealthState
	typedSequence   uint64
	startedAt       time.Time
	recorder        *diagnostics.Recorder
	reference       string
}

type componentHealthState struct {
	update   typedhealth.HealthUpdate
	sequence uint64
}

func stableCorrelationID(value string) string {
	value = strings.TrimSpace(value)
	if len(value) < 3 || len(value) > 128 {
		return "correlation_runtime"
	}
	validPrefix := false
	for _, prefix := range []string{"correlation_", "request_", "support_"} {
		if strings.HasPrefix(value, prefix) && len(value) > len(prefix) {
			validPrefix = true
			break
		}
	}
	if !validPrefix {
		return "correlation_runtime"
	}
	for _, character := range value {
		if !(character >= 'a' && character <= 'z' || character >= '0' && character <= '9' || character == '_' || character == '-') {
			return "correlation_runtime"
		}
	}
	return value
}

func NewRuntime(config Config) (*Runtime, error) {
	if config.Version == "" || len(config.Components) == 0 {
		return nil, ErrInvalidConfiguration
	}
	if config.ShutdownTimeout == 0 {
		config.ShutdownTimeout = 30 * time.Second
	}
	if config.ShutdownTimeout <= 0 {
		return nil, ErrInvalidConfiguration
	}
	seen := make(map[string]bool)
	capabilities := make([]string, 0, len(config.Components))
	for _, component := range config.Components {
		if component.Capability == "" || component.Service == nil || seen[component.Capability] {
			return nil, ErrInvalidConfiguration
		}
		seen[component.Capability] = true
		capabilities = append(capabilities, component.Capability)
	}
	return &Runtime{
		config:          config,
		state:           New,
		health:          health.New(config.Version, capabilities, config.Clock),
		typedHealth:     config.HealthTracker,
		metrics:         config.Metrics,
		eventLog:        config.EventLog,
		correlation:     stableCorrelationID(config.CorrelationID),
		typedComponents: make(map[typedhealth.Dimension]map[string]componentHealthState),
	}, nil
}

func (r *Runtime) Start(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	r.opMu.Lock()
	defer r.opMu.Unlock()
	r.mu.Lock()
	if r.state != New {
		r.mu.Unlock()
		return ErrInvalidState
	}
	r.state = Starting
	r.mu.Unlock()
	r.recorder = diagnostics.FromContext(ctx)
	r.reference = supportref.FromContext(ctx)
	if r.reference == "" {
		r.reference = supportref.New()
	}
	ctx = r.diagnosticContext(ctx)
	if r.correlation == "correlation_runtime" {
		r.correlation = r.reference
	}
	if err := ctx.Err(); err != nil {
		code, outcome, severity := lifecycleFailure(lifecycleStart, err)
		r.recordLifecycle(ctx, Component{Capability: "service", Required: true}, lifecycleStart, code, typedhealth.StatusDown, typedhealth.RetryWaitForChange, time.Time{}, outcome, severity, "Runtime start could not proceed.", err)
		r.mu.Lock()
		r.state = Failed
		r.mu.Unlock()
		r.health.SetLive(false)
		return reportedLifecycleError{cause: err}
	}
	for _, component := range r.config.Components {
		err := component.Service.Start(ctx)
		if err == nil {
			err = ctx.Err()
		}
		if err != nil {
			cleanupErr := r.cleanupFailed(ctx, component)
			code, outcome, severity := lifecycleFailure(lifecycleStart, err)
			status := typedhealth.StatusDown
			if !component.Required {
				status = typedhealth.StatusDegraded
			}
			retry, nextRetry := lifecycleRetry(r.telemetryNow(), outcome)
			if !component.Required {
				r.health.Set(component.Capability, health.Unavailable, "start_failed", 0)
				r.recordLifecycle(ctx, component, lifecycleStart, code, status, retry, nextRetry, outcome, severity, "Component failed to start.", err)
				continue
			}
			r.health.Set(component.Capability, health.Unavailable, "start_failed", 0)
			r.recordLifecycle(ctx, component, lifecycleStart, code, status, retry, nextRetry, outcome, severity, "Required component failed to start.", err)
			r.updateServiceHealth(status, code, retry, nextRetry)
			rollbackErr := r.rollback(ctx)
			r.mu.Lock()
			r.state = Failed
			r.mu.Unlock()
			r.health.SetLive(false)
			return errors.Join(fmt.Errorf("start %s: %w", component.Capability, reportedLifecycleError{cause: err}), cleanupErr, rollbackErr)
		}
		r.started = append(r.started, component)
		r.health.Set(component.Capability, health.Ready, "", 0)
		r.recordLifecycle(ctx, component, lifecycleStart, "ready", typedhealth.StatusReady, typedhealth.RetryNone, time.Time{}, sharedtelemetry.OutcomeStateChange, sharedtelemetry.SeverityInfo, "Component started.", nil)
	}
	r.mu.Lock()
	r.state = Running
	r.mu.Unlock()
	r.updateServiceHealth(typedhealth.StatusReady, "ready", typedhealth.RetryNone, time.Time{})
	r.startedAt = r.telemetryNow()
	if r.metrics != nil {
		_ = r.metrics.SetServiceUptime(r.startedAt, r.startedAt)
	}
	return nil
}

func (r *Runtime) Shutdown(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	r.health.SetLive(false)
	r.opMu.Lock()
	defer r.opMu.Unlock()
	r.mu.Lock()
	if r.state == Stopped {
		r.mu.Unlock()
		return nil
	}
	if r.state != Running && r.state != Failed {
		r.mu.Unlock()
		return ErrInvalidState
	}
	r.state = Stopping
	r.mu.Unlock()
	ctx = r.diagnosticContext(ctx)
	shutdownCtx, cancel := context.WithTimeout(ctx, r.config.ShutdownTimeout)
	defer cancel()
	var result error
	for i := len(r.started) - 1; i >= 0; i-- {
		component := r.started[i]
		err := component.Service.Shutdown(shutdownCtx)
		if err == nil {
			err = shutdownCtx.Err()
		}
		if err != nil {
			result = errors.Join(result, fmt.Errorf("shutdown %s: %w", component.Capability, reportedLifecycleError{cause: err}))
			r.health.Set(component.Capability, health.Degraded, "shutdown_failed", 0)
			code, outcome, severity := lifecycleFailure(lifecycleShutdown, err)
			retry, nextRetry := lifecycleRetry(r.telemetryNow(), outcome)
			r.recordLifecycle(ctx, component, lifecycleShutdown, code, typedhealth.StatusDegraded, retry, nextRetry, outcome, severity, "Component shutdown failed.", err)
		} else {
			r.health.Set(component.Capability, health.Unavailable, "stopped", 0)
			r.recordLifecycle(ctx, component, lifecycleShutdown, "stopped", typedhealth.StatusNotApplicable, typedhealth.RetryNone, time.Time{}, sharedtelemetry.OutcomeStateChange, sharedtelemetry.SeverityInfo, "Component stopped.", nil)
		}
	}
	r.started = nil
	if result == nil {
		r.updateServiceHealth(typedhealth.StatusNotApplicable, "stopped", typedhealth.RetryNone, time.Time{})
	} else {
		code, outcome, _ := lifecycleFailure(lifecycleShutdown, result)
		retry, nextRetry := lifecycleRetry(r.telemetryNow(), outcome)
		r.updateServiceHealth(typedhealth.StatusDegraded, code, retry, nextRetry)
	}
	r.mu.Lock()
	r.state = Stopped
	r.mu.Unlock()
	if !r.startedAt.IsZero() && r.metrics != nil {
		_ = r.metrics.SetServiceUptime(r.startedAt, r.telemetryNow())
	}
	return result
}

func (r *Runtime) State() State            { r.mu.RLock(); defer r.mu.RUnlock(); return r.state }
func (r *Runtime) Health() health.Snapshot { return r.health.Snapshot() }

// TypedHealth returns the optional shared health contract. A zero snapshot is
// returned when the runtime was constructed without typed telemetry.
func (r *Runtime) TypedHealth() typedhealth.HealthSnapshot {
	if r == nil || r.typedHealth == nil {
		return typedhealth.HealthSnapshot{}
	}
	return r.typedHealth.Snapshot()
}

func (r *Runtime) rollback(parent context.Context) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), r.config.ShutdownTimeout)
	defer cancel()
	var result error
	rollbackOutcome := "success"
	for i := len(r.started) - 1; i >= 0; i-- {
		component := r.started[i]
		err := component.Service.Shutdown(ctx)
		if err == nil {
			err = ctx.Err()
		}
		if err != nil {
			result = errors.Join(result, fmt.Errorf("rollback %s: %w", component.Capability, reportedLifecycleError{cause: err}))
			r.health.Set(component.Capability, health.Degraded, "rollback_failed", 0)
			code, outcome, severity := lifecycleFailure(lifecycleRollback, err)
			retry, nextRetry := lifecycleRetry(r.telemetryNow(), outcome)
			r.recordLifecycle(ctx, component, lifecycleRollback, code, typedhealth.StatusDegraded, retry, nextRetry, outcome, severity, "Component rollback failed.", err)
			if outcome == sharedtelemetry.OutcomeCanceled {
				rollbackOutcome = "canceled"
			}
		} else {
			r.health.Set(component.Capability, health.Unavailable, "stopped", 0)
			r.recordLifecycle(ctx, component, lifecycleRollback, "stopped", typedhealth.StatusNotApplicable, typedhealth.RetryNone, time.Time{}, sharedtelemetry.OutcomeStateChange, sharedtelemetry.SeverityInfo, "Component rolled back.", nil)
		}
	}
	r.started = nil
	if r.metrics != nil {
		if result != nil && rollbackOutcome == "success" {
			rollbackOutcome = "failed"
		}
		_ = r.metrics.RecordUpdatePhase("rollback", rollbackOutcome)
	}
	return result
}

func (r *Runtime) cleanupFailed(parent context.Context, component Component) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), r.config.ShutdownTimeout)
	defer cancel()
	err := component.Service.Shutdown(ctx)
	if err == nil {
		err = ctx.Err()
	}
	if err != nil {
		code, outcome, severity := lifecycleFailure(lifecycleRollback, err)
		retry, nextRetry := lifecycleRetry(r.telemetryNow(), outcome)
		r.recordLifecycle(ctx, component, lifecycleRollback, code, typedhealth.StatusDegraded, retry, nextRetry, outcome, severity, "Failed-start cleanup could not complete.", err)
		return fmt.Errorf("cleanup failed start %s: %w", component.Capability, reportedLifecycleError{cause: err})
	}
	return nil
}

const (
	lifecycleStart    = "component_start"
	lifecycleShutdown = "component_shutdown"
	lifecycleRollback = "component_rollback"
)

// Each wrapped cause has already passed through the component lifecycle owner.
// Command owners may still report independent failures joined alongside it.
type reportedLifecycleError struct{ cause error }

func (e reportedLifecycleError) Error() string                      { return "Runtime component lifecycle failed." }
func (e reportedLifecycleError) Unwrap() error                      { return e.cause }
func (e reportedLifecycleError) RuntimeLifecycleFailureOwned() bool { return true }

func lifecycleFailure(operation string, err error) (string, sharedtelemetry.EventOutcome, sharedtelemetry.EventSeverity) {
	prefix := strings.TrimPrefix(operation, "component_")
	if errors.Is(err, context.Canceled) {
		return prefix + "_canceled", sharedtelemetry.OutcomeCanceled, sharedtelemetry.SeverityInfo
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return prefix + "_deadline", sharedtelemetry.OutcomeFailed, sharedtelemetry.SeverityError
	}
	return prefix + "_failed", sharedtelemetry.OutcomeFailed, sharedtelemetry.SeverityError
}

func lifecycleRetry(now time.Time, outcome sharedtelemetry.EventOutcome) (typedhealth.RetryDecision, time.Time) {
	if outcome == sharedtelemetry.OutcomeStateChange || outcome == sharedtelemetry.OutcomeSuccess {
		return typedhealth.RetryNone, time.Time{}
	}
	if now.IsZero() {
		return typedhealth.RetryWaitForChange, time.Time{}
	}
	return typedhealth.RetryScheduled, now.Add(time.Second)
}

func (r *Runtime) telemetryNow() time.Time {
	if r != nil && r.config.Clock != nil {
		if now := r.config.Clock.Now(); !now.IsZero() {
			return now.UTC().Round(0)
		}
	}
	return time.Now().UTC().Round(0)
}

// diagnosticContext borrows process metadata across independently canceled
// startup, rollback and shutdown contexts. Runtime never closes the recorder.
func (r *Runtime) diagnosticContext(ctx context.Context) context.Context {
	if supportref.FromContext(ctx) == "" {
		ctx = supportref.WithContext(ctx, r.reference)
	}
	if diagnostics.FromContext(ctx) == nil && r.recorder != nil {
		ctx = diagnostics.WithRecorder(ctx, r.recorder)
	}
	return ctx
}

func (r *Runtime) recordLifecycle(ctx context.Context, component Component, operation, code string, status typedhealth.HealthStatus, retry typedhealth.RetryDecision, nextRetry time.Time, outcome sharedtelemetry.EventOutcome, severity sharedtelemetry.EventSeverity, message string, failure error) {
	dimension := componentDimension(component.Capability)
	correlation := r.correlation
	if correlation == "" {
		correlation = "correlation_runtime"
	}
	r.updateTypedHealth(component.Capability, typedhealth.HealthUpdate{
		Dimension:     dimension,
		Status:        status,
		Code:          code,
		Summary:       message,
		RepairAction:  lifecycleRepairAction(status),
		CorrelationID: correlation,
		Retry:         retry,
		NextRetryAt:   nextRetry,
	})

	if r.metrics != nil {
		_ = r.metrics.Record("paperboat_runtime_operations_total", 1, map[string]string{
			"component": metricComponent(component.Capability),
			"result":    metricResult(outcome, code),
		})
	}
	errorreport.Current().ServiceLifecycle(ctx, component.Capability, operation, failure)
	if failure == nil && r.recorder != nil {
		_ = r.recorder.RecordWithSupportReference(operation, code, "info", supportref.FromContext(ctx), map[string]string{
			"component": "paperboat-daemon", "operation": operation,
			"capability": string(dimension), "outcome": string(outcome), "retry_class": string(retry),
		})
	}
	if r.eventLog == nil {
		return
	}
	now := r.telemetryNow()
	_, _ = r.eventLog.Record(sharedtelemetry.EventInput{
		At:            now,
		Severity:      severity,
		Component:     dimension,
		Name:          operation,
		Code:          code,
		Outcome:       outcome,
		Message:       message,
		CorrelationID: correlation,
		IDs: sharedtelemetry.SafeIDs{
			ResourceID: resourceID(component.Capability),
			SessionID:  "session_runtime",
			ProcessID:  "process_runtime",
			ConfigID:   configID(r.config.Version),
		},
		Retry:       retry,
		NextRetryAt: nextRetry,
	})
}

func (r *Runtime) updateServiceHealth(status typedhealth.HealthStatus, code string, retry typedhealth.RetryDecision, nextRetry time.Time) {
	if r == nil {
		return
	}
	correlation := r.correlation
	if correlation == "" {
		correlation = "correlation_runtime"
	}
	r.updateTypedHealth("runtime_service", typedhealth.HealthUpdate{
		Dimension:     typedhealth.DimensionService,
		Status:        status,
		Code:          code,
		Summary:       "Host runtime service state changed.",
		RepairAction:  lifecycleRepairAction(status),
		CorrelationID: correlation,
		Retry:         retry,
		NextRetryAt:   nextRetry,
	})
}

func (r *Runtime) updateTypedHealth(capability string, update typedhealth.HealthUpdate) {
	if r == nil || r.typedHealth == nil {
		return
	}
	r.typedMu.Lock()
	defer r.typedMu.Unlock()
	if r.typedComponents == nil {
		r.typedComponents = make(map[typedhealth.Dimension]map[string]componentHealthState)
	}
	components := r.typedComponents[update.Dimension]
	if components == nil {
		components = make(map[string]componentHealthState)
		r.typedComponents[update.Dimension] = components
	}
	r.typedSequence++
	components[capability] = componentHealthState{update: update, sequence: r.typedSequence}
	aggregate := aggregateHealthUpdate(components)
	previous := r.typedHealth.Snapshot().Dimensions.Get(update.Dimension)
	if err := r.typedHealth.Update(aggregate); err != nil {
		return
	}
	if r.metrics == nil {
		return
	}
	snapshot := r.typedHealth.Snapshot()
	current := snapshot.Dimensions.Get(update.Dimension)
	if previous.Status != current.Status || previous.Code != current.Code {
		_ = r.metrics.RecordHealthTransition(update.Dimension, previous.Status, current.Status)
	}
	_ = r.metrics.RecordHealth(snapshot)
}

// ReportComponentStep records a stable host component's start failure (retried after retryAfter, or not
// retried when it is 0) or its recovery (nil err) in the typed health that pb doctor reads.
func (r *Runtime) ReportComponentStep(capability string, err error, retryAfter time.Duration) {
	if r == nil {
		return
	}
	correlation := r.correlation
	if correlation == "" {
		correlation = "correlation_runtime"
	}
	update := typedhealth.HealthUpdate{Dimension: componentDimension(capability), Status: typedhealth.StatusReady, Code: "ready",
		Summary: "Host component " + capability + " is running.", RepairAction: "No action is required.", CorrelationID: correlation, Retry: typedhealth.RetryNone}
	if err != nil {
		update.Status, update.Code = typedhealth.StatusDegraded, "start_failed"
		update.Summary = "Host component " + capability + " failed to start: " + err.Error()
		update.RepairAction = "Restart the Paperboat host service, then run pb doctor again."
		update.Retry = typedhealth.RetryWaitForChange
		if retryAfter > 0 {
			update.RepairAction = "Paperboat retries this on its own. If it keeps failing, restart the Paperboat host service, then run pb doctor again."
			update.Retry, update.NextRetryAt = typedhealth.RetryScheduled, r.telemetryNow().Add(retryAfter)
		}
	}
	r.updateTypedHealth(capability, update)
}

func aggregateHealthUpdate(components map[string]componentHealthState) typedhealth.HealthUpdate {
	var selected componentHealthState
	selectedRank := -1
	for _, candidate := range components {
		rank := healthStatusRank(candidate.update.Status)
		if rank > selectedRank || rank == selectedRank && candidate.sequence > selected.sequence {
			selected = candidate
			selectedRank = rank
		}
	}
	return selected.update
}

func healthStatusRank(status typedhealth.HealthStatus) int {
	switch status {
	case typedhealth.StatusDown:
		return 5
	case typedhealth.StatusDegraded:
		return 4
	case typedhealth.StatusUnknown:
		return 3
	case typedhealth.StatusReady:
		return 2
	case typedhealth.StatusNotApplicable:
		return 1
	default:
		return 0
	}
}

func componentDimension(capability string) typedhealth.Dimension {
	normalized := strings.ToLower(strings.TrimSpace(capability))
	normalized = strings.NewReplacer(".", "_", "-", "_").Replace(normalized)
	switch {
	case normalized == "edge" || strings.Contains(normalized, "connector") || strings.Contains(normalized, "transport") || strings.Contains(normalized, "tunnel") || strings.Contains(normalized, "peer"):
		return typedhealth.DimensionEdge
	case normalized == "config" || strings.Contains(normalized, "config"):
		return typedhealth.DimensionConfig
	case normalized == "route" || strings.Contains(normalized, "route"):
		return typedhealth.DimensionRoute
	case normalized == "origin" || strings.Contains(normalized, "origin") || normalized == "target" || strings.Contains(normalized, "preview") || strings.Contains(normalized, "serve"):
		return typedhealth.DimensionOrigin
	case normalized == "dns" || strings.Contains(normalized, "dns"):
		return typedhealth.DimensionDNS
	case normalized == "certificate" || strings.Contains(normalized, "cert") || strings.Contains(normalized, "tls"):
		return typedhealth.DimensionCertificate
	case normalized == "access" || strings.Contains(normalized, "authoriz") || normalized == "auth" || strings.Contains(normalized, "admission"):
		return typedhealth.DimensionAccess
	case normalized == "update" || strings.Contains(normalized, "update"):
		return typedhealth.DimensionUpdate
	default:
		return typedhealth.DimensionService
	}
}

func lifecycleRepairAction(status typedhealth.HealthStatus) string {
	if status == typedhealth.StatusReady || status == typedhealth.StatusNotApplicable {
		return "No action is required."
	}
	return "Retry or restart the affected host component."
}

func metricComponent(capability string) string {
	normalized := strings.ToLower(strings.TrimSpace(capability))
	switch {
	case strings.Contains(normalized, "protocol"):
		return "protocol"
	case strings.Contains(normalized, "auth") || strings.Contains(normalized, "access") || strings.Contains(normalized, "admission"):
		return "auth"
	case strings.Contains(normalized, "session"):
		return "session"
	case strings.Contains(normalized, "upload") || strings.Contains(normalized, "transfer"):
		return "upload"
	case strings.Contains(normalized, "preview") || strings.Contains(normalized, "target") || strings.Contains(normalized, "serve"):
		return "preview"
	case strings.Contains(normalized, "connector") || strings.Contains(normalized, "edge") || strings.Contains(normalized, "transport") || strings.Contains(normalized, "tunnel") || strings.Contains(normalized, "peer"):
		return "connector"
	case strings.Contains(normalized, "update"):
		return "update"
	case strings.Contains(normalized, "storage"):
		return "storage"
	default:
		return "service"
	}
}

func metricResult(outcome sharedtelemetry.EventOutcome, code string) string {
	if strings.HasSuffix(code, "_deadline") {
		return "deadline"
	}
	switch outcome {
	case sharedtelemetry.OutcomeCanceled:
		return "canceled"
	case sharedtelemetry.OutcomeFailed, sharedtelemetry.OutcomeRejected:
		return "unavailable"
	default:
		return "ok"
	}
}

func opaqueIDPart(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var builder strings.Builder
	for _, character := range value {
		if character >= 'a' && character <= 'z' || character >= '0' && character <= '9' || character == '_' || character == '-' {
			builder.WriteRune(character)
		} else {
			builder.WriteByte('_')
		}
		if builder.Len() >= 64 {
			break
		}
	}
	part := strings.Trim(builder.String(), "_")
	if len(part) < 2 {
		part = "runtime"
	}
	return part
}

func resourceID(capability string) string { return "resource_" + opaqueIDPart(capability) }
func configID(version string) string      { return "config_" + opaqueIDPart(version) }
