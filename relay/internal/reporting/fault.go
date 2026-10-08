package reporting

import (
	"context"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/getsentry/sentry-go"
	"github.com/getsentry/sentry-go/attribute"
	"github.com/pinksaucepasta/paperboat-relay/derpquic"
	"github.com/pinksaucepasta/paperboat-relay/nodelifecycle"
)

// Fault is a structured, privacy-safe projection of a service failure. Err
// values are classified by type only; their Error strings are never emitted.
type Fault struct {
	Schema           string    `json:"schema"`
	At               time.Time `json:"at"`
	Severity         string    `json:"severity"`
	Component        string    `json:"component"`
	Operation        string    `json:"operation"`
	Stage            string    `json:"stage"`
	Name             string    `json:"name"`
	Code             string    `json:"code"`
	Cause            string    `json:"cause"`
	ErrorType        string    `json:"error_type"`
	ErrorChain       []string  `json:"error_chain,omitempty"`
	Outcome          string    `json:"outcome"`
	SupportReference string    `json:"support_reference"`
	CorrelationID    string    `json:"correlation_id"`
	Errno            int       `json:"errno,omitempty"`
	HTTPStatus       int       `json:"http_status,omitempty"`
	SourceFile       string    `json:"source_file,omitempty"`
	SourceFunction   string    `json:"source_function,omitempty"`
	SourceLine       int       `json:"source_line,omitempty"`
}

type failureDefinition struct {
	operation string
	stage     string
	name      string
	code      string
	outcome   string
}

var failureDefinitions = map[string]failureDefinition{
	"selfhost_command":     {operation: "service_lifecycle", stage: "selfhost", name: "selfhost_failed", code: "selfhost_command_failed"},
	"selfhost_child":       {operation: "service_lifecycle", stage: "selfhost", name: "selfhost_failed", code: "selfhost_child_failed"},
	"selfhost_certificate": {operation: "dependency_health", stage: "selfhost", name: "selfhost_failed", code: "selfhost_certificate_failed"},
	"selfhost_claim":       {operation: "service_lifecycle", stage: "selfhost", name: "selfhost_failed", code: "selfhost_claim_failed"},
	"control_request":      {operation: "dependency_health", stage: "control_request", name: "relay_control_request_failed", code: "control_request_failed"},
	"service_config":       {operation: "service_lifecycle", stage: "configure", name: "relay_service_setup_failed", code: "service_setup_failed", outcome: "rejected"},
	"service_build":        {operation: "service_lifecycle", stage: "configure", name: "relay_service_setup_failed", code: "service_setup_failed"},
	"service_start":        {operation: "service_lifecycle", stage: "startup", name: "relay_service_start_failed", code: "service_start_failed"},
	"service_run":          {operation: "service_lifecycle", stage: "serve", name: "relay_service_failed", code: "service_failed"},
	"process_panic":        {operation: "service_lifecycle", stage: "process", name: "relay_process_panicked", code: "process_panic"},
}

type supportReferenceContextKey struct{}

func WithSupportReference(ctx context.Context, reference string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if !validReference(reference) {
		return ctx
	}
	return context.WithValue(ctx, supportReferenceContextKey{}, reference)
}

func SupportReference(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	reference, _ := ctx.Value(supportReferenceContextKey{}).(string)
	if validReference(reference) {
		return reference
	}
	return ""
}

// CaptureFailure writes one local structured event even when Sentry is
// disabled or unreachable. The same generated/caller reference is used by the
// local event and Sentry envelope. Unknown definition keys collapse to a
// producer-owned safe bucket.
func (r *Reporter) CaptureFailure(ctx context.Context, definition string, err error) Fault {
	return r.observeFailure(ctx, definition, err, true)
}

// ObserveFailure records a recoverable attempt locally and as structured SDK
// telemetry. The final service owner alone decides whether to capture an exception.
func (r *Reporter) ObserveFailure(ctx context.Context, definition string, err error) Fault {
	return r.observeFailure(ctx, definition, err, false)
}

func (r *Reporter) observeFailure(ctx context.Context, definition string, err error, capture bool) Fault {
	if err == nil {
		return Fault{}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	d, ok := failureDefinitions[definition]
	if !ok {
		d = failureDefinition{operation: "service_lifecycle", stage: "unknown", name: "relay_failure", code: "internal_failure"}
	}
	reference := SupportReference(ctx)
	if reference == "" {
		reference = Reference()
	}
	if reference == "" {
		return Fault{}
	}
	component := "paperboat-relay"
	if r != nil && (r.component == "paperboat-relay" || r.component == "paperboat-tunnel" || r.component == "paperboat-selfhost") {
		component = r.component
	}
	fault := projectFault(component, d, reference, err)
	fault.SupportReference = reference
	fault.CorrelationID = reference
	if pc, file, line, ok := runtime.Caller(2); ok {
		fault.SourceFile = safeSourceFile(filepath.Base(file))
		fault.SourceLine = line
		if function := runtime.FuncForPC(pc); function != nil {
			fault.SourceFunction = safeSourceFunction(function.Name())
		}
	}
	if r != nil && r.localFault != nil {
		r.localFault(fault)
	} else {
		writeLocalFault(fault)
	}
	if !r.beginSubmit() {
		return fault
	}
	defer r.submitMu.RUnlock()
	ctx = sentry.SetHubOnContext(ctx, r.hub)
	r.captureFaultLog(ctx, fault)
	if r.metrics && r.permit(&r.metricsSent, &r.droppedMetrics, maxDetailMetrics) {
		meter := sentry.NewMeter(ctx)
		meter.SetAttributes(attribute.String("component", r.component), attribute.String("operation", fault.Operation), attribute.String("outcome", fault.Outcome), attribute.String("code", fault.Code))
		meter.Count("paperboat.operation.count", 1)
	}
	if !capture || fault.Outcome == "canceled" || fault.Outcome == "rejected" || !r.permit(&r.sent, &r.droppedErrors, maxEvents) {
		return fault
	}
	event := &sentry.Event{
		Level: sentry.LevelError, Message: "paperboat service failure",
		Tags: map[string]string{
			"operation": fault.Operation, "stage": fault.Stage, "name": fault.Name,
			"code": fault.Code, "cause": fault.Cause, "error_type": fault.ErrorType,
			"outcome": fault.Outcome, "support_reference": fault.SupportReference,
		},
		Exception:   []sentry.Exception{{Type: fault.Code, Value: fault.Cause, Stacktrace: stack(2)}},
		Fingerprint: []string{"{{ default }}", fault.Component, fault.Stage, fault.Code, fault.Cause},
	}
	addFaultTags(event.Tags, fault)
	r.client.CaptureEvent(event, nil, nil)
	return fault
}

func (r *Reporter) captureFaultLog(ctx context.Context, fault Fault) {
	if r == nil || !r.enabled || r.hub == nil || !r.logs || !r.permit(&r.logsSent, &r.droppedLogs, 128) {
		return
	}
	ctx = sentry.SetHubOnContext(ctx, r.hub)
	logger := sentry.NewLogger(ctx)
	entry := logger.Error()
	if fault.Severity == "info" {
		entry = logger.Info()
	}
	attrs := []attribute.Builder{
		attribute.String("component", fault.Component), attribute.String("operation", fault.Operation),
		attribute.String("stage", fault.Stage), attribute.String("name", fault.Name),
		attribute.String("outcome", fault.Outcome), attribute.String("code", fault.Code),
		attribute.String("cause", fault.Cause), attribute.String("error_type", fault.ErrorType),
		attribute.String("support_reference", fault.SupportReference), attribute.String("correlation_id", fault.CorrelationID),
		attribute.String("error_chain", strings.Join(fault.ErrorChain, ",")),
	}
	if fault.Errno > 0 {
		attrs = append(attrs, attribute.String("errno", strconv.Itoa(fault.Errno)))
	}
	if fault.HTTPStatus >= http.StatusBadRequest && fault.HTTPStatus <= 599 {
		attrs = append(attrs, attribute.String("http_status", strconv.Itoa(fault.HTTPStatus)))
	}
	if fault.SourceFile != "" {
		attrs = append(attrs, attribute.String("source_file", fault.SourceFile))
	}
	if fault.SourceFunction != "" {
		attrs = append(attrs, attribute.String("source_function", fault.SourceFunction))
	}
	if fault.SourceLine > 0 {
		attrs = append(attrs, attribute.String("source_line", strconv.Itoa(fault.SourceLine)))
	}
	logger.SetAttributes(attrs...)
	if fault.Severity == "warning" {
		entry = logger.Warn()
	}
	entry.Emit("paperboat.operation")
}

func projectFault(component string, definition failureDefinition, reference string, err error) Fault {
	fault := Fault{
		Schema: "paperboat.edge_event.v1", At: time.Now().UTC(), Severity: "error",
		Component: component, Operation: definition.operation, Stage: definition.stage,
		Name: definition.name, Code: definition.code, Cause: "internal", ErrorType: "error",
		Outcome: "failed", SupportReference: reference, CorrelationID: reference,
	}
	fault.Cause, fault.ErrorType, fault.ErrorChain, fault.Errno, fault.HTTPStatus = projectError(err)
	if definition.outcome != "" {
		fault.Outcome = definition.outcome
		if definition.outcome == "rejected" {
			fault.Severity = "warning"
		}
	}
	if fault.Cause == "context_canceled" {
		fault.Outcome, fault.Severity = "canceled", "info"
	} else if fault.HTTPStatus >= http.StatusBadRequest && fault.HTTPStatus < http.StatusInternalServerError {
		fault.Outcome, fault.Severity = "rejected", "warning"
	}
	return fault
}

func projectError(err error) (cause, errorType string, errorChain []string, errno, httpStatus int) {
	cause, errorType = "internal", "error"
	if r := reflect.TypeOf(err); r != nil {
		errorType = safeErrorType(r)
	}
	remaining := []error{err}
	seen := make(map[error]bool)
	priority := 0
	unresolved := false
	for count := 0; len(remaining) > 0 && count < 16; count++ {
		current := remaining[0]
		remaining = remaining[1:]
		if current == nil {
			continue
		}
		typ := reflect.TypeOf(current)
		if typ != nil && typ.Comparable() {
			if seen[current] {
				if !sameError(current, context.Canceled) && !sameError(current, context.DeadlineExceeded) {
					unresolved = true
				}
				continue
			}
			seen[current] = true
		}
		name := safeErrorType(typ)
		if len(errorChain) < 8 && (len(errorChain) == 0 || errorChain[len(errorChain)-1] != name) {
			errorChain = append(errorChain, name)
		}
		value := reflect.ValueOf(current)
		if value.Kind() == reflect.Pointer && value.IsNil() {
			unresolved = true
			continue
		}
		classifiedCause, rank := classifyCause(current)
		if rank > priority {
			cause, priority = classifiedCause, rank
			errno, httpStatus = 0, 0
			if value, ok := current.(syscall.Errno); ok {
				errno = int(value)
			}
			if status, ok := current.(interface{ DiagnosticStatus() int }); ok {
				value := status.DiagnosticStatus()
				if value >= http.StatusBadRequest && value <= 599 {
					httpStatus = value
				}
			}
		}
		leaf := true
		switch wrapped := current.(type) {
		case interface{ Unwrap() []error }:
			children := wrapped.Unwrap()
			if len(children) > 16-count {
				unresolved = true
				children = children[:16-count]
			}
			for _, child := range children {
				if child != nil {
					leaf = false
				}
			}
			remaining = append(remaining, children...)
		case interface{ Unwrap() error }:
			child := wrapped.Unwrap()
			leaf = child == nil
			remaining = append(remaining, child)
		}
		if leaf && rank == 0 && priority < 2 {
			cause, priority, errno, httpStatus = "internal", 2, 0, 0
		}
	}
	// A bounded, incomplete or cyclic chain cannot establish normal cancellation.
	if cause == "context_canceled" && (unresolved || len(remaining) > 0) {
		cause, errno, httpStatus = "internal", 0, 0
	}
	if len(errorChain) > 0 {
		errorType = errorChain[0]
	}
	return cause, errorType, errorChain, errno, httpStatus
}

// SafeFailureCause projects a bounded cause enum for console diagnostics.
func SafeFailureCause(err error) string {
	cause, _, _, _, _ := projectError(err)
	return cause
}

func safeErrorType(typ reflect.Type) string {
	if typ == nil {
		return "error"
	}
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	name := typ.Name()
	if name == "" || len(name) > 64 {
		return "error"
	}
	for _, character := range name {
		if !(character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || character == '_') {
			return "error"
		}
	}
	return name
}

func classifyCause(err error) (string, int) {
	if _, ok := err.(interface{ processPanic() }); ok {
		return "process_panic", 95
	}
	if sameError(err, context.Canceled) {
		return "context_canceled", 1
	}
	if sameError(err, context.DeadlineExceeded) {
		return "deadline_exceeded", 90
	}
	if status, ok := err.(interface{ DiagnosticStatus() int }); ok {
		code := status.DiagnosticStatus()
		switch code {
		case http.StatusUnauthorized, http.StatusForbidden:
			return "permission_denied", 88
		case http.StatusNotFound:
			return "not_found", 88
		case http.StatusConflict:
			return "conflict", 88
		case http.StatusTooManyRequests:
			return "rate_limited", 88
		case http.StatusServiceUnavailable:
			return "service_unavailable", 88
		default:
			if code >= http.StatusBadRequest && code < http.StatusInternalServerError {
				return "invalid_request", 88
			}
			if code >= http.StatusInternalServerError && code <= 599 {
				return "upstream_failure", 88
			}
		}
	}
	if sameError(err, nodelifecycle.ErrFenced) {
		return "authority_rejected", 85
	}
	if sameError(err, nodelifecycle.ErrControl) {
		return "authority_unavailable", 40
	}
	if sameError(err, derpquic.ErrAdmission) {
		return "authorization_rejected", 83
	}
	if sameError(err, derpquic.ErrProtocol) {
		return "protocol_rejected", 83
	}
	if sameError(err, derpquic.ErrOverload) {
		return "capacity_exhausted", 83
	}
	if sameError(err, derpquic.ErrExpired) {
		return "authority_expired", 83
	}
	if sameError(err, derpquic.ErrClosed) {
		return "relay_closed", 82
	}
	if sameError(err, http.ErrServerClosed) {
		return "server_closed", 81
	}
	if typed, ok := err.(*derpquic.Error); ok {
		switch typed.Code {
		case 1:
			return "authorization_rejected", 83
		case 2:
			return "protocol_rejected", 83
		case 3:
			return "capacity_exhausted", 83
		case 4:
			return "authority_expired", 83
		}
	}
	if sameError(err, os.ErrNotExist) {
		return "not_found", 65
	}
	if sameError(err, os.ErrPermission) {
		return "permission_denied", 65
	}
	if sameError(err, fs.ErrNotExist) {
		return "not_found", 65
	}
	if sameError(err, fs.ErrPermission) {
		return "permission_denied", 65
	}
	if sameError(err, net.ErrClosed) {
		return "connection_closed", 65
	}
	if sameError(err, io.EOF) {
		return "end_of_stream", 65
	}
	if sameError(err, io.ErrUnexpectedEOF) {
		return "truncated_stream", 65
	}
	if errno, ok := err.(syscall.Errno); ok {
		switch errno {
		case syscall.ENOENT:
			return "not_found", 64
		case syscall.EACCES, syscall.EPERM:
			return "permission_denied", 64
		case syscall.ECONNREFUSED:
			return "connection_refused", 64
		case syscall.ECONNRESET:
			return "connection_reset", 64
		case syscall.EPIPE:
			return "broken_pipe", 64
		case syscall.ENETUNREACH, syscall.EHOSTUNREACH:
			return "network_unreachable", 64
		case syscall.EADDRINUSE:
			return "address_in_use", 64
		case syscall.EINVAL:
			return "invalid_argument", 64
		case syscall.EMFILE, syscall.ENFILE, syscall.ENOSPC, syscall.ENOMEM:
			return "resource_exhausted", 64
		case syscall.ETIMEDOUT:
			return "network_timeout", 64
		}
		return "system_error", 60
	}
	if dns, ok := err.(*net.DNSError); ok {
		if dns.IsTimeout {
			return "network_timeout", 50
		}
		return "dns_failure", 50
	}
	if network, ok := err.(net.Error); ok && network.Timeout() {
		return "network_timeout", 40
	}
	return "internal", 0
}

func sameError(err, target error) bool {
	if err == nil || target == nil {
		return false
	}
	typ := reflect.TypeOf(err)
	return typ.Comparable() && typ == reflect.TypeOf(target) && err == target
}

func validStage(stage string) bool {
	switch stage {
	case "selfhost", "control_request", "serve", "configure", "startup", "process", "unknown":
		return true
	default:
		return false
	}
}

func failureName(stage string) string {
	switch stage {
	case "selfhost":
		return "selfhost_failed"
	case "control_request":
		return "relay_control_request_failed"
	case "serve":
		return "relay_service_failed"
	case "configure":
		return "relay_service_setup_failed"
	case "startup":
		return "relay_service_start_failed"
	case "process":
		return "relay_process_panicked"
	default:
		return "relay_failure"
	}
}

func validCause(cause string) bool {
	switch cause {
	case "internal", "process_panic", "context_canceled", "deadline_exceeded", "authority_unavailable", "authority_rejected", "authorization_rejected", "protocol_rejected", "capacity_exhausted", "authority_expired", "relay_closed", "server_closed", "not_found", "permission_denied", "conflict", "rate_limited", "service_unavailable", "invalid_request", "upstream_failure", "connection_closed", "end_of_stream", "truncated_stream", "connection_refused", "connection_reset", "broken_pipe", "network_unreachable", "address_in_use", "invalid_argument", "resource_exhausted", "network_timeout", "dns_failure", "system_error":
		return true
	default:
		return false
	}
}

func validErrorType(value string) bool {
	if value == "" || len(value) > 64 {
		return false
	}
	for _, character := range value {
		if !(character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || character == '_') {
			return false
		}
	}
	return true
}

func validTypeChain(value string) bool {
	if value == "" {
		return false
	}
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

func validSourceFunction(value string) bool {
	return value != "" && len(value) <= 64 && safeSourceFunction(value) == value
}

func safeSourceFile(value string) string {
	if value == "" || len(value) > 128 || value != filepath.Base(value) {
		return ""
	}
	for _, r := range value {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' || r == '.') {
			return ""
		}
	}
	return value
}

func addFaultTags(tags map[string]string, fault Fault) {
	tags["correlation_id"] = fault.CorrelationID
	if len(fault.ErrorChain) > 0 {
		tags["error_chain"] = strings.Join(fault.ErrorChain, ",")
	}
	if fault.Errno > 0 {
		tags["errno"] = strconv.Itoa(fault.Errno)
	}
	if fault.HTTPStatus >= http.StatusBadRequest && fault.HTTPStatus <= 599 {
		tags["http_status"] = strconv.Itoa(fault.HTTPStatus)
	}
	if fault.SourceFile != "" {
		tags["source_file"] = fault.SourceFile
	}
	if fault.SourceFunction != "" {
		tags["source_function"] = fault.SourceFunction
	}
	if fault.SourceLine > 0 {
		tags["source_line"] = strconv.Itoa(fault.SourceLine)
	}
}

var localLoggerOnce sync.Once
var localLogger *slog.Logger

func writeLocalFault(fault Fault) {
	localLoggerOnce.Do(func() {
		localLogger = slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{ReplaceAttr: func(_ []string, value slog.Attr) slog.Attr {
			if value.Key == slog.TimeKey {
				return slog.Attr{}
			}
			return value
		}}))
	})
	level := slog.LevelError
	if fault.Severity == "info" {
		level = slog.LevelInfo
	} else if fault.Severity == "warning" {
		level = slog.LevelWarn
	}
	attrs := []slog.Attr{
		slog.String("schema", fault.Schema), slog.Time("at", fault.At),
		slog.String("severity", fault.Severity), slog.String("component", fault.Component),
		slog.String("operation", fault.Operation), slog.String("stage", fault.Stage),
		slog.String("name", fault.Name), slog.String("code", fault.Code),
		slog.String("cause", fault.Cause), slog.String("error_type", fault.ErrorType),
		slog.Any("error_chain", fault.ErrorChain), slog.String("outcome", fault.Outcome),
		slog.String("support_reference", fault.SupportReference), slog.String("correlation_id", fault.CorrelationID),
	}
	if fault.Errno > 0 {
		attrs = append(attrs, slog.Int("errno", fault.Errno))
	}
	if fault.HTTPStatus >= http.StatusBadRequest && fault.HTTPStatus <= 599 {
		attrs = append(attrs, slog.Int("http_status", fault.HTTPStatus))
	}
	if fault.SourceFile != "" {
		attrs = append(attrs, slog.String("source_file", fault.SourceFile))
	}
	if fault.SourceFunction != "" {
		attrs = append(attrs, slog.String("source_function", fault.SourceFunction))
	}
	if fault.SourceLine > 0 {
		attrs = append(attrs, slog.Int("source_line", fault.SourceLine))
	}
	localLogger.LogAttrs(context.Background(), level, fault.Name, attrs...)
}
