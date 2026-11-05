package reporting

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/getsentry/sentry-go"
	"github.com/getsentry/sentry-go/attribute"
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
	// Path is the control API path a failed request used (a fixed route, never an id or query).
	Path           string `json:"path,omitempty"`
	SourceFile     string `json:"source_file,omitempty"`
	SourceFunction string `json:"source_function,omitempty"`
	SourceLine     int    `json:"source_line,omitempty"`
}

// Definition is one named failure a service reports: where it happened and the bounded labels it carries.
// Name may contain %s, replaced by the service name (relay, tunnel) of the reporting component.
type Definition struct {
	Operation string
	Stage     string
	Name      string
	Code      string
	Outcome   string
}

var failureDefinitions = map[string]Definition{
	"control_request": {Operation: "dependency_health", Stage: "control_request", Name: "%s_control_request_failed", Code: "control_request_failed"},
	"service_config":  {Operation: "service_lifecycle", Stage: "configure", Name: "%s_service_setup_failed", Code: "service_setup_failed", Outcome: "rejected"},
	"service_build":   {Operation: "service_lifecycle", Stage: "configure", Name: "%s_service_setup_failed", Code: "service_setup_failed"},
	"service_start":   {Operation: "service_lifecycle", Stage: "startup", Name: "%s_service_start_failed", Code: "service_start_failed"},
	"service_run":     {Operation: "service_lifecycle", Stage: "serve", Name: "%s_service_failed", Code: "service_failed"},
	"process_panic":   {Operation: "service_lifecycle", Stage: "process", Name: "%s_process_panicked", Code: "process_panic"},
}

// PanicFailure classifies a recovered panic without retaining its value.
type PanicFailure struct{}

func (PanicFailure) Error() string { return "process panicked" }
func (PanicFailure) processPanic() {}

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
		d = Definition{Operation: "service_lifecycle", Stage: "unknown", Name: "%s_failure", Code: "internal_failure"}
	}
	reference := SupportReference(ctx)
	if reference == "" {
		reference = requestReference(err)
	}
	if reference == "" {
		reference = Reference()
	}
	if reference == "" {
		return Fault{}
	}
	component := "paperboat-relay"
	if r != nil && components[r.component] {
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
	if fault.Path != "" {
		attrs = append(attrs, attribute.String("path", fault.Path))
	}
	logger.SetAttributes(attrs...)
	if fault.Severity == "warning" {
		entry = logger.Warn()
	}
	entry.Emit("paperboat.operation")
}

func projectFault(component string, definition Definition, reference string, err error) Fault {
	fault := Fault{
		Schema: "paperboat.edge_event.v1", At: time.Now().UTC(), Severity: "error",
		Component: component, Operation: definition.Operation, Stage: definition.Stage,
		Name: serviceName(component, definition.Name), Code: definition.Code, Cause: "internal", ErrorType: "error",
		Outcome: "failed", SupportReference: reference, CorrelationID: reference,
	}
	// Project's walk is bounded (an error chain may cycle); it also finds the request path.
	var located interface{ DiagnosticPath() string }
	p := Project(err, Options{Visit: func(node error) {
		if located == nil {
			located, _ = node.(interface{ DiagnosticPath() string })
		}
	}})
	fault.Cause, fault.ErrorType, fault.ErrorChain, fault.Errno, fault.HTTPStatus = p.Cause, p.ErrorType, p.ErrorChain, p.Errno, p.HTTPStatus
	if located != nil && safeRoutePath(located.DiagnosticPath()) {
		fault.Path = located.DiagnosticPath()
	}
	if definition.Outcome != "" {
		fault.Outcome = definition.Outcome
		if definition.Outcome == "rejected" {
			fault.Severity = "warning"
		}
	}
	if fault.Cause == "context_canceled" {
		fault.Outcome, fault.Severity = "canceled", "info"
	} else if p.Rejected {
		fault.Outcome, fault.Severity = "rejected", "warning"
	}
	return fault
}

// SafeErrorMetadata returns bounded enum/type metadata for a wrapped error; it never evaluates Error.
func SafeErrorMetadata(err error) (cause, errorType string) {
	p := Project(err, Options{})
	return p.Cause, p.ErrorType
}

// SafeFailureDetails returns bounded enum/type/status metadata for user-facing diagnostics.
func SafeFailureDetails(err error) (cause, errorType string, httpStatus int) {
	p := Project(err, Options{})
	return p.Cause, p.ErrorType, p.HTTPStatus
}

// SafeFailureCause projects a bounded cause enum for console diagnostics.
func SafeFailureCause(err error) string {
	return Project(err, Options{}).Cause
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

func sameError(err, target error) bool {
	if err == nil || target == nil {
		return false
	}
	typ := reflect.TypeOf(err)
	return typ.Comparable() && typ == reflect.TypeOf(target) && err == target
}

func validStage(stage string) bool {
	_, ok := stageNames[stage]
	return ok
}

func failureName(component, stage string) string {
	name, ok := stageNames[stage]
	if !ok {
		name = stageNames["unknown"]
	}
	return serviceName(component, name)
}

func validCause(cause string) bool { return causes[cause] }

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

// safeRoutePath admits only a fixed API route: lower-case words, no query; prefixed ids (route_…) are refused.
func safeRoutePath(path string) bool {
	if len(path) > 96 || !strings.HasPrefix(path, "/v1/") {
		return false
	}
	for _, r := range path {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '/' && r != '-' {
			return false
		}
	}
	return true
}

func writeLocalFault(fault Fault) {
	// The destination is os.Stderr at the time of the fault, not at the first fault of the process.
	localLogger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{ReplaceAttr: func(_ []string, value slog.Attr) slog.Attr {
		if value.Key == slog.TimeKey {
			return slog.Attr{}
		}
		return value
	}}))
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
	if fault.Path != "" {
		attrs = append(attrs, slog.String("path", fault.Path))
	}
	localLogger.LogAttrs(context.Background(), level, fault.Name, attrs...)
}
