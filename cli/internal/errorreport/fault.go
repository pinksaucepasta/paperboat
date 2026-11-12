package errorreport

import (
	"context"
	"github.com/pinksaucepasta/paperboat/shared/reporting"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/getsentry/sentry-go"
	"github.com/getsentry/sentry-go/attribute"
	"github.com/pinksaucepasta/paperboat/shared/supportref"
)

// Fault contains only producer-owned classifications and metadata derived from
// error types. It never contains an error message, address, path or payload.
type Fault struct {
	Component, Operation, Stage, Code, Cause, ErrorType, SupportReference string
	ServiceComponent                                                      string
	SourceFile, SourceFunction                                            string
	SourceLine                                                            int
	Outcome, Severity                                                     string
	ErrorChain                                                            []string
	Errno                                                                 int
	HTTPStatus                                                            int
}

type faultObserver struct{ observe func(context.Context, Fault) }

var localFaultObserver atomic.Pointer[faultObserver]

// InstallFaultObserver binds the process-owned nonblocking local recorder. The
// callback must only enqueue safe metadata; it must not perform disk/network I/O.
func InstallFaultObserver(observe func(context.Context, Fault)) func() {
	var next *faultObserver
	if observe != nil {
		next = &faultObserver{observe: observe}
	}
	previous := localFaultObserver.Swap(next)
	return func() { localFaultObserver.CompareAndSwap(next, previous) }
}

// ProjectFault traverses at most sixteen error nodes, including joined errors.
// The finite traversal also handles cyclic wrappers without calling Error().
func ProjectFault(ctx context.Context, component, operation, stage, code string, err error) (f Fault) {
	if err == nil || !safeComponent(component) || !safeClassification(stage) || !safeClassification(code) {
		return Fault{}
	}
	f = Fault{Component: Component(component), Operation: Operation(operation), Stage: faultStage(stage), Code: faultCode(code), Cause: "internal", ErrorType: "error", SupportReference: supportref.FromContext(ctx), Outcome: "failed", Severity: "error"}
	if ctx != nil {
		if value, ok := ctx.Value(lifecycleComponentKey{}).(string); ok {
			f.ServiceComponent = serviceComponent(value)
		}
	}
	if f.SupportReference == "" {
		f.SupportReference = supportref.New()
	}
	// Error implementations are outside the reporter's ownership. A broken
	// diagnostic method or Unwrap must not interrupt cleanup or suppress the
	// failure. Keep its reference and classify an incomplete projection as a fault.
	defer func() {
		if recover() != nil {
			f.Cause, f.ErrorType = "internal", "error"
			f.Outcome, f.Severity = "failed", "error"
			f.Errno, f.HTTPStatus = 0, 0
		}
	}()
	p := reporting.Project(err, reporting.Options{Classify: []reporting.NodeClassifier{remoteCause}, Visit: func(current error) {
		if staged, ok := current.(interface{ DiagnosticStage() string }); ok {
			if stage := faultStage(staged.DiagnosticStage()); stage != "unknown" {
				f.Stage = stage
			}
		}
		if coded, ok := current.(interface{ DiagnosticCode() string }); ok {
			if code := faultCode(coded.DiagnosticCode()); code != "internal_failure" {
				f.Code = code
			}
		}
	}})
	f.Cause, f.ErrorType, f.ErrorChain, f.Errno, f.HTTPStatus = p.Cause, p.ErrorType, p.ErrorChain, p.Errno, p.HTTPStatus
	if p.Rejected {
		f.Outcome, f.Severity = "rejected", "warning"
	}
	if f.Cause == "context_canceled" {
		f.Outcome, f.Severity = "canceled", "info"
	}
	return f
}

// remoteCause reads the cause a remote or protected diagnostic carries, if it is in the finite vocabulary.
func remoteCause(err error) (string, int, int) {
	if remote, ok := err.(interface {
		DiagnosticCause() string
		DiagnosticErrno() int
	}); ok {
		cause, number := remote.DiagnosticCause(), remote.DiagnosticErrno()
		if ValidCause(cause) && number >= 0 && number <= 65535 {
			rank := 60
			switch cause {
			case "context_canceled":
				rank = 1
			case "internal":
				rank = 2
			case "deadline_exceeded":
				rank = 90
			}
			return cause, number, rank
		}
	}
	return "", 0, 0
}

// ValidCause validates the finite cause vocabulary used by local and protected wire diagnostics.
func ValidCause(cause string) bool {
	switch cause {
	case "invalid_request", "internal", "context_canceled", "deadline_exceeded", "not_found", "permission_denied", "conflict", "rate_limited", "service_unavailable", "upstream_failure", "system_error", "connection_refused", "connection_reset", "broken_pipe", "network_unreachable", "address_in_use", "invalid_argument", "resource_exhausted", "network_timeout", "dns_failure", "connection_closed", "end_of_stream", "truncated_stream":
		return true
	}
	return false
}

// CaptureFailure preserves local evidence independently of export configuration
// and quotas. The returned fault carries the same reference as the Sentry event.
func (r *Reporter) CaptureFailure(ctx context.Context, component, operation, stage, code string, err error) Fault {
	if ctx == nil {
		ctx = context.Background()
	}
	if r != nil && supportref.FromContext(ctx) == "" {
		if reference := r.ambientReference.Load(); reference != nil {
			ctx = supportref.WithContext(ctx, *reference)
		}
	}
	return r.captureFault(ctx, ProjectFault(ctx, component, operation, stage, code, err), true)
}

// ObserveFailure records an attempt without creating an exception. The final
// failure owner uses CaptureFailure when an unexpected fault requires capture.
func (r *Reporter) ObserveFailure(ctx context.Context, component, operation, stage, code string, err error) Fault {
	if ctx == nil {
		ctx = context.Background()
	}
	if r != nil && supportref.FromContext(ctx) == "" {
		if reference := r.ambientReference.Load(); reference != nil {
			ctx = supportref.WithContext(ctx, *reference)
		}
	}
	return r.captureFault(ctx, ProjectFault(ctx, component, operation, stage, code, err), false)
}

func (r *Reporter) captureFault(ctx context.Context, f Fault, capture bool) Fault {
	if f.Code == "" {
		return f
	}
	if pc, file, line, ok := runtime.Caller(2); ok {
		f.SourceFile, f.SourceLine = filepath.Base(file), line
		if function := runtime.FuncForPC(pc); function != nil {
			f.SourceFunction = reporting.SafeSourceFunction(function.Name())
		}
	}
	ctx = supportref.WithContext(ctx, f.SupportReference)
	if observer := localFaultObserver.Load(); observer != nil {
		copy := f
		copy.ErrorChain = append([]string(nil), f.ErrorChain...)
		observer.observe(ctx, copy)
	}
	if !r.beginSubmit(false) {
		return f
	}
	defer r.submitMu.RUnlock()
	ctx = r.context(ctx)
	if r.logs && r.admitSignal(0) {
		logger := sentry.NewLogger(ctx)
		logger.SetAttributes(attribute.String("component", f.Component), attribute.String("operation", f.Operation), attribute.String("outcome", f.Outcome), attribute.String("stage", f.Stage), attribute.String("code", f.Code), attribute.String("cause", f.Cause), attribute.String("error_type", f.ErrorType), attribute.String("support_reference", f.SupportReference))
		if f.ServiceComponent != "" {
			logger.SetAttributes(attribute.String("service_component", f.ServiceComponent))
		}
		if f.Errno != 0 {
			logger.SetAttributes(attribute.String("errno", strconv.Itoa(f.Errno)))
		}
		if f.HTTPStatus >= 400 && f.HTTPStatus <= 599 {
			logger.SetAttributes(attribute.String("http_status", strconv.Itoa(f.HTTPStatus)))
		}
		if len(f.ErrorChain) > 0 {
			logger.SetAttributes(attribute.String("error_chain", strings.Join(f.ErrorChain, ",")))
		}
		logger.SetAttributes(attribute.String("source_file", f.SourceFile), attribute.String("source_function", f.SourceFunction), attribute.String("source_line", strconv.Itoa(f.SourceLine)))
		if f.Severity == "info" {
			logger.Info().Emit("paperboat.operation")
		} else if f.Severity == "warning" {
			logger.Warn().Emit("paperboat.operation")
		} else {
			logger.Error().Emit("paperboat.operation")
		}
	}
	duration := int64(-1)
	if attempt, _ := ctx.Value(attemptFaultKey{}).(*attemptFault); attempt != nil {
		duration = attempt.duration.Load()
	}
	metricItems := uint64(1)
	if duration >= 0 {
		metricItems++
	}
	if r.metrics && r.admitSignalItems(1, metricItems) {
		meter := sentry.NewMeter(ctx)
		meter.SetAttributes(attribute.String("component", f.Component), attribute.String("operation", f.Operation), attribute.String("outcome", f.Outcome))
		meter.Count("paperboat.operation.count", 1)
		if duration >= 0 {
			meter.Distribution("paperboat.operation.duration", time.Duration(duration).Seconds(), sentry.WithUnit(sentry.UnitSecond))
		}
	}
	if !capture || f.Outcome == "canceled" || f.Outcome == "rejected" || !r.admit(time.Now()) {
		return f
	}
	event := sentry.NewEvent()
	event.Level = sentry.LevelError
	event.Tags = map[string]string{"component": f.Component, "operation": f.Operation, "stage": f.Stage, "code": f.Code, "cause": f.Cause, "error_type": f.ErrorType, "support_reference": f.SupportReference}
	event.Tags["source_file"], event.Tags["source_function"], event.Tags["source_line"] = f.SourceFile, f.SourceFunction, strconv.Itoa(f.SourceLine)
	if f.ServiceComponent != "" {
		event.Tags["service_component"] = f.ServiceComponent
	}
	if f.Errno != 0 {
		event.Tags["errno"] = strconv.Itoa(f.Errno)
	}
	if f.HTTPStatus >= 400 && f.HTTPStatus <= 599 {
		event.Tags["http_status"] = strconv.Itoa(f.HTTPStatus)
	}
	if len(f.ErrorChain) > 0 {
		event.Tags["error_chain"] = strings.Join(f.ErrorChain, ",")
	}
	event.Exception = []sentry.Exception{{Type: f.Code, Value: f.Cause, Stacktrace: safeStack(2)}}
	event.Fingerprint = []string{"{{ default }}", f.Component, f.Stage, f.Code, f.Cause}
	if span := sentry.SpanFromContext(ctx); span != nil {
		event.Contexts = map[string]sentry.Context{"trace": {"trace_id": span.TraceID, "span_id": span.SpanID}}
	}
	r.client.CaptureEvent(event, nil, nil)
	return f
}

func faultStage(value string) string {
	switch value {
	case "uninstall_prepare", "uninstall_lock", "uninstall_recover", "uninstall_sessions", "uninstall_activator", "uninstall_services", "uninstall_power", "uninstall_journal", "uninstall_files", "uninstall_lock_close", "native_uninstall", "account_sweep", "machine_guard", "shared_payload_parents", "final_inventory", "reset_cli", "control_request", "local_gateway", "grant_issue", "peer_authority", "peer_connect", "stream_open", "target_connect", "reconciliation", "lifecycle", "component_start", "component_shutdown", "component_rollback", "command", "process", "diagnostic_storage", "delivery", "listener_bind", "listener_accept", "update_check", "update_download", "update_install", "relay_targets", "relay_probe", "relay_report", "pin_cleanup":
		return value
	}
	return "unknown"
}
func faultCode(value string) string {
	switch value {
	case "local_access_guard_connect_failed", "local_access_native_listener_failed", "local_access_browser_http_listener_failed", "local_access_browser_https_listener_failed", "local_access_browser_aliases_failed", "local_access_browser_certificate_failed", "local_access_browser_proxy_failed", "local_access_machine_names_failed":
		return value
	case "purge_uninstall_prepare_failed", "purge_uninstall_lock_failed", "purge_uninstall_recover_failed", "purge_uninstall_sessions_failed", "purge_uninstall_activator_failed", "purge_uninstall_services_failed", "purge_uninstall_power_failed", "purge_uninstall_journal_failed", "purge_uninstall_files_failed", "purge_uninstall_lock_close_failed", "purge_native_uninstall_failed", "purge_account_sweep_failed", "purge_machine_guard_failed", "purge_shared_payload_parents_failed", "purge_final_inventory_failed", "purge_reset_cli_failed", "control_request_failed", "relay_latency_failed", "local_gateway_failed", "native_private_failed", "local_access_failed", "service_failed", "transport_failed", "process_panic", "unexpected_cli_failure", "unexpected_failure", "internal_failure", "command_failed", "exec_operation_failed", "exec_persistence_failed", "terminal_session_failed", "environment_sync_failed", "diagnostic_storage_unavailable", "file_transfer_failed", "managed_ssh_failed", "peer_authority_failed", "availability_apply_failed", "config_sync_failed", "updater_invalid_request", "updater_custom_installation", "updater_approval_required", "updater_candidate_changed", "updater_active_terminal_sessions", "updater_activation_unavailable", "updater_recovery_required", "updater_release_not_found", "updater_update_failed", "updater_check_failed":
		return value
	}
	if lifecycleCode(value) == value {
		return value
	}
	return "internal_failure"
}

func validTypeChain(value string) bool {
	types := strings.Split(value, ",")
	if len(types) > 8 {
		return false
	}
	for _, name := range types {
		if !reporting.ValidErrorType(name) {
			return false
		}
	}
	return true
}
func validErrno(value string) bool {
	number, err := strconv.ParseUint(value, 10, 32)
	return err == nil && number != 0 && strconv.FormatUint(number, 10) == value
}

func validHTTPStatus(value string) bool {
	number, err := strconv.Atoi(value)
	return err == nil && number >= 400 && number <= 599 && strconv.Itoa(number) == value
}

func validSourceFile(value string) bool {
	if value == "" || len(value) > 128 || value != filepath.Base(value) {
		return false
	}
	return strings.IndexFunc(value, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' || r == '.')
	}) == -1
}
