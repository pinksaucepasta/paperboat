package errorreport

import (
	"context"
	"io"
	"io/fs"
	"net"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/getsentry/sentry-go"
	"github.com/getsentry/sentry-go/attribute"
	"github.com/pinksaucepasta/paperboat/internal/supportref"
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
func ProjectFault(ctx context.Context, component, operation, stage, code string, err error) Fault {
	if err == nil || !safeComponent(component) || !safeClassification(stage) || !safeClassification(code) {
		return Fault{}
	}
	f := Fault{Component: Component(component), Operation: Operation(operation), Stage: faultStage(stage), Code: faultCode(code), Cause: "internal", ErrorType: "error", SupportReference: supportref.FromContext(ctx), Outcome: "failed", Severity: "error"}
	if ctx != nil {
		if value, ok := ctx.Value(lifecycleComponentKey{}).(string); ok {
			f.ServiceComponent = serviceComponent(value)
		}
	}
	if f.SupportReference == "" {
		f.SupportReference = supportref.New()
	}
	remaining := []error{err}
	seen := make(map[error]bool)
	priority := 0
	httpPriority, operationalPriority := 0, 0
	operationalCause, operationalErrno, operationalType := "internal", 0, "error"
	for count := 0; len(remaining) > 0 && count < 16; count++ {
		current := remaining[0]
		remaining = remaining[1:]
		if current == nil {
			continue
		}
		t := reflect.TypeOf(current)
		if t.Comparable() {
			if seen[current] {
				continue
			}
			seen[current] = true
		}
		name := faultType(t)
		if len(f.ErrorChain) < 8 && (len(f.ErrorChain) == 0 || f.ErrorChain[len(f.ErrorChain)-1] != name) {
			f.ErrorChain = append(f.ErrorChain, name)
		}
		if value := reflect.ValueOf(current); value.Kind() == reflect.Pointer && value.IsNil() {
			continue
		}
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
		cause, errno, rank := nodeCause(current)
		status := 0
		if coded, ok := current.(interface{ DiagnosticStatus() int }); ok {
			status = coded.DiagnosticStatus()
		}
		if status >= 400 && status <= 599 {
			statusRank := rank
			if status >= 500 {
				statusRank++
			}
			if statusRank > httpPriority {
				f.HTTPStatus, httpPriority = status, statusRank
			}
		}
		if rank > priority {
			f.Cause, f.Errno, f.ErrorType, priority = cause, errno, name, rank
		}
		if rank > operationalPriority && cause != "context_canceled" && !(status >= 400 && status < 500) {
			operationalCause, operationalErrno, operationalType, operationalPriority = cause, errno, name, rank
		}
		switch wrapped := current.(type) {
		case interface{ Unwrap() []error }:
			children := wrapped.Unwrap()
			if len(children) > 16-count {
				children = children[:16-count]
			}
			remaining = append(remaining, children...)
		case interface{ Unwrap() error }:
			remaining = append(remaining, wrapped.Unwrap())
		}
	}
	if f.HTTPStatus >= 400 && f.HTTPStatus < 500 {
		if rejectionOnly(err) {
			f.Outcome, f.Severity = "rejected", "warning"
		} else {
			f.Cause, f.Errno, f.ErrorType = operationalCause, operationalErrno, operationalType
		}
	}
	if f.Cause == "context_canceled" && !cancellationOnly(err) {
		f.Cause = "internal"
	}
	if f.Cause == "context_canceled" {
		f.Outcome, f.Severity = "canceled", "info"
	}
	return f
}

// Rejection is expected only when every bounded terminal branch is a
// protocol rejection or ordinary cancellation. A status wrapper cannot hide
// an independent storage, decoding or network failure beneath it.
func rejectionOnly(err error) bool {
	pending := []error{err}
	sawStatus := false
	for visited := 0; len(pending) > 0; visited++ {
		if visited >= 16 {
			return false
		}
		current := pending[0]
		pending = pending[1:]
		if current == nil {
			return false
		}
		value := reflect.ValueOf(current)
		if value.Kind() == reflect.Pointer && value.IsNil() {
			return false
		}
		if value.Type().Comparable() && current == context.Canceled {
			continue
		}
		rejected := false
		if coded, ok := current.(interface{ DiagnosticStatus() int }); ok {
			status := coded.DiagnosticStatus()
			rejected = status >= 400 && status < 500
		}
		switch wrapped := current.(type) {
		case interface{ Unwrap() []error }:
			children := wrapped.Unwrap()
			if len(children) == 0 || len(children)+len(pending) > 15-visited {
				return false
			}
			pending = append(pending, children...)
		case interface{ Unwrap() error }:
			child := wrapped.Unwrap()
			if child == nil && rejected {
				sawStatus = true
				continue
			}
			pending = append(pending, child)
		default:
			if !rejected {
				return false
			}
			sawStatus = true
		}
	}
	return sawStatus
}

// Cancellation is orderly only when every bounded terminal cause is canceled.
// Unresolved cycles, oversized joins and typed nils cannot prove that claim.
func cancellationOnly(err error) bool {
	remaining := []error{err}
	for visited := 0; len(remaining) > 0; visited++ {
		if visited >= 16 {
			return false
		}
		current := remaining[0]
		remaining = remaining[1:]
		if current == nil {
			return false
		}
		value := reflect.ValueOf(current)
		if value.Kind() == reflect.Pointer && value.IsNil() {
			return false
		}
		if value.Type().Comparable() && current == context.Canceled {
			continue
		}
		switch wrapped := current.(type) {
		case interface{ Unwrap() []error }:
			children := wrapped.Unwrap()
			if len(children) == 0 || len(children)+len(remaining) > 15-visited {
				return false
			}
			remaining = append(remaining, children...)
		case interface{ Unwrap() error }:
			remaining = append(remaining, wrapped.Unwrap())
		default:
			return false
		}
	}
	return true
}

func faultType(t reflect.Type) string {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	name := t.Name()
	if name == "" || len(name) > 64 {
		return "error"
	}
	for _, r := range name {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_') {
			return "error"
		}
	}
	return name
}

func nodeCause(err error) (string, int, int) {
	if reflect.TypeOf(err).Comparable() {
		if err == context.Canceled {
			return "context_canceled", 0, 1
		}
		if err == context.DeadlineExceeded {
			return "deadline_exceeded", 0, 90
		}
		if err == fs.ErrNotExist {
			return "not_found", 0, 60
		}
		if err == fs.ErrPermission {
			return "permission_denied", 0, 60
		}
		if err == net.ErrClosed {
			return "connection_closed", 0, 60
		}
		if err == io.EOF {
			return "end_of_stream", 0, 60
		}
		if err == io.ErrUnexpectedEOF {
			return "truncated_stream", 0, 60
		}
	}
	if status, ok := err.(interface{ DiagnosticStatus() int }); ok {
		switch status.DiagnosticStatus() {
		case 401, 403:
			return "permission_denied", 0, 70
		case 404:
			return "not_found", 0, 70
		case 409:
			return "conflict", 0, 70
		case 429:
			return "rate_limited", 0, 70
		case 503:
			return "service_unavailable", 0, 70
		default:
			if status.DiagnosticStatus() >= 400 && status.DiagnosticStatus() < 500 {
				return "invalid_request", 0, 70
			}
			if status.DiagnosticStatus() >= 500 && status.DiagnosticStatus() <= 599 {
				return "upstream_failure", 0, 70
			}
		}
	}
	if errno, ok := err.(syscall.Errno); ok {
		cause := "system_error"
		// Errno.Is understands native Windows file/path/ACL errors as well as
		// Unix errno values. Comparing only POSIX constants loses that mapping.
		switch {
		case errno.Is(fs.ErrNotExist):
			return "not_found", int(errno), 60
		case errno.Is(fs.ErrPermission):
			return "permission_denied", int(errno), 60
		case errno.Is(fs.ErrExist):
			return "conflict", int(errno), 60
		}
		switch errno {
		case syscall.ENOENT:
			cause = "not_found"
		case syscall.EACCES, syscall.EPERM:
			cause = "permission_denied"
		case syscall.ECONNREFUSED:
			cause = "connection_refused"
		case syscall.ECONNRESET:
			cause = "connection_reset"
		case syscall.EPIPE:
			cause = "broken_pipe"
		case syscall.ENETUNREACH, syscall.EHOSTUNREACH:
			cause = "network_unreachable"
		case syscall.EADDRINUSE:
			cause = "address_in_use"
		case syscall.EINVAL:
			cause = "invalid_argument"
		case syscall.EMFILE, syscall.ENFILE, syscall.ENOSPC, syscall.ENOMEM:
			cause = "resource_exhausted"
		case syscall.ETIMEDOUT:
			cause = "network_timeout"
		}
		return cause, int(errno), 60
	}
	if dns, ok := err.(*net.DNSError); ok {
		if dns.IsTimeout {
			return "network_timeout", 0, 50
		}
		return "dns_failure", 0, 50
	}
	if network, ok := err.(net.Error); ok && network.Timeout() {
		return "network_timeout", 0, 40
	}
	// Wrappers do not turn a pure cancellation into a substantive failure.
	// An unknown terminal cause does: joining it with cleanup cancellation
	// must not hide the failure or suppress its exception.
	switch err.(type) {
	case interface{ Unwrap() error }, interface{ Unwrap() []error }:
		return "internal", 0, 0
	default:
		return "internal", 0, 2
	}
}

func validCause(cause string) bool {
	switch cause {
	case "invalid_request", "internal", "context_canceled", "deadline_exceeded", "not_found", "permission_denied", "conflict", "rate_limited", "service_unavailable", "upstream_failure", "system_error", "connection_refused", "connection_reset", "broken_pipe", "network_unreachable", "address_in_use", "invalid_argument", "resource_exhausted", "network_timeout", "dns_failure", "connection_closed", "end_of_stream", "truncated_stream":
		return true
	}
	return false
}

func validErrorType(value string) bool {
	return value != "" && len(value) <= 64 && strings.IndexFunc(value, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_')
	}) == -1
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
			f.SourceFunction = safeSourceFunction(function.Name())
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
	case "control_request", "local_gateway", "grant_issue", "peer_authority", "peer_connect", "stream_open", "target_connect", "reconciliation", "lifecycle", "component_start", "component_shutdown", "component_rollback", "command", "process", "diagnostic_storage", "delivery", "listener_bind", "listener_accept":
		return value
	}
	return "unknown"
}
func faultCode(value string) string {
	switch value {
	case "control_request_failed", "local_gateway_failed", "native_private_failed", "local_access_failed", "service_failed", "transport_failed", "process_panic", "unexpected_cli_failure", "unexpected_failure", "internal_failure", "command_failed", "exec_operation_failed", "exec_persistence_failed", "terminal_session_failed", "environment_sync_failed", "diagnostic_storage_unavailable", "file_transfer_failed", "managed_ssh_failed", "peer_authority_failed", "availability_apply_failed", "config_sync_failed":
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
		if !validErrorType(name) {
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

func safeSourceFunction(value string) string {
	if slash := strings.LastIndexByte(value, '/'); slash >= 0 {
		value = value[slash+1:]
	}
	value = strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '.' {
			return r
		}
		return -1
	}, value)
	if len(value) > 64 {
		value = value[:64]
	}
	return value
}

func validSourceFile(value string) bool {
	if value == "" || len(value) > 128 || value != filepath.Base(value) {
		return false
	}
	return strings.IndexFunc(value, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' || r == '.')
	}) == -1
}
