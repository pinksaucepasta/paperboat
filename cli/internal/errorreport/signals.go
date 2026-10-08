package errorreport

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/getsentry/sentry-go"
	"github.com/getsentry/sentry-go/attribute"
	"github.com/pinksaucepasta/paperboat/internal/supportref"
)

type signalLimit struct {
	window  time.Time
	count   uint64
	dropped uint64
}

var process atomic.Pointer[Reporter]

// Install binds the process-owned exporter. The caller owns Flush and restoration.
func Install(r *Reporter) func() {
	old := process.Swap(r)
	return func() { process.CompareAndSwap(r, old) }
}
func Current() *Reporter { return process.Load() }
func safeComponent(s string) bool {
	return s == "paperboat-cli" || s == "paperboat-daemon" || s == "pb" || s == "paperboatd"
}
func Component(s string) string {
	if s == "paperboatd" || s == "paperboat-daemon" {
		return "paperboat-daemon"
	}
	return "paperboat-cli"
}
func Operation(s string) string {
	switch s {
	case "bandwidth", "peer_stream", "peer_probe", "machine_pairing", "preview_observation", "private_authorization", "preview_attachment", "runtime_observation", "authorization_keys", "browser_authorization", "revocation_refresh", "accessor_discovery", "helper_enrollment", "config_sync", "identity_renewal", "machine_control", "peer_identity", "environment_enrollment", "tunnel_enrollment", "connector_admission", "tunnel_bootstrap", "inspector_authorization", "component_start", "component_shutdown", "component_rollback", "route_transition", "exec", "sftp", "rsync", "environments", "team", "desktop", "ping", "relay", "auth", "inbox", "session", "sessions", "machine", "pair", "setup", "uninstall", "install", "send", "transfer", "wait", "bugreport", "resolve", "tag", "approve", "service", "edge", "route", "origin", "dns", "certificate", "access", "login", "logout", "project", "workspace", "connect", "terminal", "ssh", "scp", "serve", "preview", "tunnel", "env", "vault", "config", "sync", "update", "doctor", "status", "version", "help", "daemon", "control_request", "runtime_lifecycle", "diagnostic":
		return s
	}
	if strings.HasPrefix(s, "__runtime-") {
		return "daemon"
	}
	return "command"
}
func Outcome(s string) string {
	switch s {
	case "success", "failed", "rejected", "canceled", "state_change":
		return s
	case "ok", "ready", "recovered":
		return "success"
	case "failure", "error", "timeout", "unavailable":
		return "failed"
	}
	return "state_change"
}
func (r *Reporter) admitSignal(signal int) bool {
	return r.admitSignalItems(signal, 1)
}

func (r *Reporter) admitSignalItems(signal int, items uint64) bool {
	// Callers hold the submission gate. Already admitted work must complete
	// even when shutdown has closed admission to subsequent producers.
	if r == nil || r.client == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	l := &r.limits[signal]
	now := time.Now()
	if l.window.IsZero() || now.Sub(l.window) >= time.Minute {
		l.window = now
		l.count = 0
	}
	limits := [3]uint64{120, detailMetricLimit, 120}
	if items > limits[signal]-l.count {
		l.dropped += items
		return false
	}
	l.count += items
	return true
}

// Dropped returns locally rejected log, metric item and trace counts.
func (r *Reporter) Dropped() [3]uint64 {
	if r == nil {
		return [3]uint64{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return [3]uint64{r.limits[0].dropped, r.limits[1].dropped, r.limits[2].dropped}
}
func (r *Reporter) context(ctx context.Context) context.Context {
	if supportref.FromContext(ctx) == "" {
		if ref := r.ambientReference.Load(); ref != nil {
			ctx = supportref.WithContext(ctx, *ref)
		}
	}
	return sentry.SetHubOnContext(ctx, sentry.NewHub(r.client, sentry.NewScope()))
}
func (r *Reporter) Observe(ctx context.Context, component, operation, outcome string, duration time.Duration) {
	if !r.beginSubmit(false) {
		return
	}
	defer r.submitMu.RUnlock()
	component = Component(component)
	operation = Operation(operation)
	outcome = Outcome(outcome)
	ctx = r.context(ctx)
	attrs := []attribute.Builder{attribute.String("component", component), attribute.String("operation", operation), attribute.String("outcome", outcome)}
	if r.logs && r.admitSignal(0) {
		a := append([]attribute.Builder{}, attrs...)
		code := operationCode(outcome)
		if lifecycle, ok := ctx.Value(lifecycleCodeKey{}).(string); ok {
			code = lifecycleCode(lifecycle)
		}
		a = append(a, attribute.String("code", code))
		if component, ok := ctx.Value(lifecycleComponentKey{}).(string); ok {
			a = append(a, attribute.String("service_component", component))
		}
		if dimension, ok := ctx.Value(lifecycleDimensionKey{}).(string); ok {
			a = append(a, attribute.String("dimension", dimension))
		}
		if ref := supportref.FromContext(ctx); ref != "" {
			a = append(a, attribute.String("support_reference", ref))
		}
		logger := sentry.NewLogger(ctx)
		logger.SetAttributes(a...)
		if outcome == "failed" {
			logger.Error().Emit("paperboat.operation")
		} else {
			logger.Info().Emit("paperboat.operation")
		}
	}
	metricItems := uint64(1)
	if duration >= 0 {
		metricItems++
	}
	if r.metrics && r.admitSignalItems(1, metricItems) {
		m := sentry.NewMeter(ctx)
		m.SetAttributes(attrs...)
		m.Count("paperboat.operation.count", 1)
		if duration >= 0 {
			m.Distribution("paperboat.operation.duration", duration.Seconds(), sentry.WithUnit(sentry.UnitSecond))
		}
	}
}
func (r *Reporter) Start(ctx context.Context, component, operation string) (context.Context, func(string)) {
	if r != nil {
		processComponent := Component(component)
		r.component.CompareAndSwap(nil, &processComponent)
		if ref := supportref.FromContext(ctx); ref != "" {
			r.ambientReference.CompareAndSwap(nil, &ref)
		}
	}
	if !r.beginSubmit(false) {
		return ctx, func(string) {}
	}
	defer r.submitMu.RUnlock()
	operation = Operation(operation)
	component = Component(component)
	r.component.CompareAndSwap(nil, &component)
	if ref := supportref.FromContext(ctx); ref != "" {
		r.ambientReference.CompareAndSwap(nil, &ref)
	}
	started := time.Now()
	ctx = r.context(ctx)
	var span *sentry.Span
	if r.rate > 0 && operation != "daemon" && r.admitSignal(2) {
		if sentry.SpanFromContext(ctx) != nil {
			span = sentry.StartSpan(ctx, "paperboat.operation", sentry.WithDescription(operation))
		} else {
			span = sentry.StartTransaction(ctx, operation, sentry.WithOpName("paperboat.operation"))
		}
		span.SetTag("component", component)
		span.SetTag("operation", operation)
		if ref := supportref.FromContext(ctx); ref != "" {
			span.SetTag("support_reference", ref)
		}
		ctx = span.Context()
	}
	var done atomic.Bool
	return ctx, func(outcome string) {
		if done.Swap(true) {
			return
		}
		outcome = Outcome(outcome)
		alreadyObserved, _ := ctx.Value(attemptFaultKey{}).(*attemptFault)
		if alreadyObserved == nil || !alreadyObserved.observed.Load() {
			r.Observe(ctx, component, operation, outcome, time.Since(started))
		}
		if span != nil {
			if !r.beginSubmit(false) {
				return
			}
			defer r.submitMu.RUnlock()
			span.SetTag("outcome", outcome)
			if outcome == "failed" {
				span.Status = sentry.SpanStatusInternalError
			} else if outcome == "canceled" {
				span.Status = sentry.SpanStatusCanceled
			} else if outcome == "rejected" {
				span.Status = sentry.SpanStatusPermissionDenied
			} else {
				span.Status = sentry.SpanStatusOK
			}
			span.Finish()
		}
	}
}

// Transport instruments only requests to this client's configured control origin.
// Redirects and upload URLs never inherit the trace header to another origin.
func Transport(base http.RoundTripper, origin string) http.RoundTripper {
	return TransportOperation(base, origin, "control_request")
}
func TransportOperation(base http.RoundTripper, origin, operation string) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	u, err := url.Parse(origin)
	if err != nil {
		return base
	}
	return ownedTransport{base: base, scheme: u.Scheme, host: u.Host, operation: Operation(operation)}
}

type ownedTransport struct {
	base         http.RoundTripper
	scheme, host string
	operation    string
}

func (t ownedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r := Current()
	owned := req.URL.Scheme == t.scheme && req.URL.Host == t.host
	clone := req.Clone(req.Context())
	clone.Header.Del("sentry-trace")
	clone.Header.Del("baggage")
	if !owned {
		clone.Header.Del(supportref.Header)
		return t.base.RoundTrip(clone)
	}
	reference := clone.Header.Get(supportref.Header)
	if !supportref.Valid(reference) {
		reference = supportref.FromContext(req.Context())
	}
	if reference == "" && r != nil {
		if ambient := r.ambientReference.Load(); ambient != nil {
			reference = *ambient
		}
	}
	if !supportref.Valid(reference) {
		reference = supportref.New()
	}
	clone.Header.Set(supportref.Header, reference)
	requestContext := supportref.WithContext(req.Context(), reference)
	clone = clone.WithContext(requestContext)
	component := "paperboat-cli"
	if r != nil {
		if value := r.component.Load(); value != nil {
			component = *value
		}
	}
	observed := &attemptFault{}
	observed.duration.Store(-1)
	requestContext = context.WithValue(requestContext, attemptFaultKey{}, observed)
	ctx, end := r.Start(requestContext, component, t.operation)
	clone = clone.WithContext(ctx)
	if ref := supportref.FromContext(ctx); ref != "" && clone.Header.Get(supportref.Header) == "" {
		clone.Header.Set(supportref.Header, ref)
	}
	if span := sentry.SpanFromContext(ctx); span != nil {
		clone.Header.Set("sentry-trace", span.ToSentryTrace())
	}
	started := time.Now()
	response, err := t.base.RoundTrip(clone)
	if response == nil && err == nil {
		err = errors.New("control transport returned no response")
	}
	if response != nil {
		// Preserve the transport's recorded attempt context even when a custom
		// RoundTripper omits Request or supplies a different one.
		copy := *response
		copy.Request = clone
		response = &copy
	}
	observed.duration.Store(int64(time.Since(started)))
	outcome := "success"
	if err != nil {
		outcome = "failed"
		if ctx.Err() != nil {
			outcome = "canceled"
		}
	} else if response.StatusCode >= 500 {
		outcome = "failed"
	} else if response.StatusCode >= 400 {
		outcome = "rejected"
	}
	if err != nil || response.StatusCode >= 400 {
		observed.observed.Store(true)
	}
	if err != nil {
		r.ObserveFailure(ctx, component, t.operation, "control_request", "control_request_failed", err)
		err = recordedHTTPFailure(err)
	} else if response.StatusCode >= 400 {
		r.ObserveFailure(ctx, component, t.operation, "control_request", "control_request_failed", responseFault(response.StatusCode))
	}
	end(outcome)
	return response, err
}

type attemptFaultKey struct{}
type attemptFault struct {
	observed atomic.Bool
	duration atomic.Int64
}

type responseFault int

func (f responseFault) Error() string         { return "control request rejected" }
func (f responseFault) DiagnosticStatus() int { return int(f) }

func cleanAttributes(in map[string]attribute.Value, metric bool) map[string]attribute.Value {
	out := map[string]attribute.Value{}
	for k, v := range in {
		s := v.AsString()
		switch k {
		case "component":
			if safeComponent(s) {
				out[k] = attribute.StringValue(Component(s))
			}
		case "operation":
			out[k] = attribute.StringValue(Operation(s))
		case "outcome":
			out[k] = attribute.StringValue(Outcome(s))
		case "stage":
			if !metric && faultStage(s) == s {
				out[k] = v
			}
		case "cause":
			if !metric && validCause(s) {
				out[k] = v
			}
		case "error_type":
			if !metric && validErrorType(s) {
				out[k] = v
			}
		case "error_chain":
			if !metric && validTypeChain(s) {
				out[k] = v
			}
		case "errno":
			if !metric && validErrno(s) {
				out[k] = v
			}
		case "source_file":
			if !metric && validSourceFile(s) {
				out[k] = v
			}
		case "source_function":
			if !metric && s != "" && safeSourceFunction(s) == s {
				out[k] = v
			}
		case "source_line":
			if !metric && validErrno(s) {
				out[k] = v
			}
		case "http_status":
			if !metric && validHTTPStatus(s) {
				out[k] = v
			}
		case "service_component":
			if !metric && ServiceComponent(s) == s {
				out[k] = v
			}
		case "dimension":
			if !metric && lifecycleDimension(s) == s {
				out[k] = v
			}
		case "code":
			if !metric && (s == "operation_failed" || s == "operation_rejected" || s == "operation_canceled" || s == "operation_succeeded" || s == "state_changed" || lifecycleCode(s) == s || faultCode(s) == s) {
				out[k] = v
			}
		case "sentry.release", "sentry.environment":
			if safeRelease(s) {
				out[k] = v
			}
		case "support_reference":
			if !metric && supportref.Valid(s) {
				out[k] = v
			}
		}
	}
	return out
}
func sanitizeLog(l *sentry.Log) *sentry.Log {
	if l == nil || l.Body != "paperboat.operation" {
		return nil
	}
	return &sentry.Log{Timestamp: l.Timestamp, TraceID: l.TraceID, SpanID: l.SpanID, Level: l.Level, Severity: l.Severity, Body: l.Body, Attributes: cleanAttributes(l.Attributes, false)}
}
func (r *Reporter) sanitizeMetric(m *sentry.Metric) *sentry.Metric {
	if m == nil {
		return nil
	}
	attrs := cleanAttributes(m.Attributes, true)
	if m.Name != "paperboat.operation.count" && m.Name != "paperboat.operation.duration" {
		r.mu.Lock()
		schema, ok := r.metricSchema[m.Name]
		if !ok {
			r.mu.Unlock()
			return nil
		}
		for key, values := range schema {
			v, present := m.Attributes["dimension."+key]
			if !present || !values[v.AsString()] {
				r.mu.Unlock()
				return nil
			}
			attrs["dimension."+key] = v
		}
		r.mu.Unlock()
	}
	m.Attributes = attrs
	m.TraceID = sentry.TraceID{}
	m.SpanID = sentry.SpanID{}
	return m
}
func sanitizeTransaction(e *sentry.Event, _ *sentry.EventHint) *sentry.Event {
	if e == nil {
		return nil
	}
	tags := map[string]string{}
	for k, v := range e.Tags {
		switch k {
		case "component":
			if safeComponent(v) {
				tags[k] = Component(v)
			}
		case "operation":
			tags[k] = Operation(v)
		case "outcome":
			tags[k] = Outcome(v)
		case "support_reference":
			if supportref.Valid(v) {
				tags[k] = v
			}
		}
	}
	contexts := safeTraceContext(e.Contexts)
	if trace := contexts["trace"]; trace != nil {
		trace["op"] = "paperboat.operation"
		trace["status"] = safeSpanStatus(e.Contexts["trace"]["status"])
		if parent, ok := e.Contexts["trace"]["parent_span_id"].(sentry.SpanID); ok {
			trace["parent_span_id"] = parent
		}
	}
	for _, s := range e.Spans {
		s.Description = Operation(s.Description)
		s.Data = nil
		s.Tags = nil
		s.Op = "paperboat.operation"
		s.Status = safeSpanStatus(s.Status)
	}
	return &sentry.Event{Type: e.Type, EventID: e.EventID, Timestamp: e.Timestamp, StartTime: e.StartTime, Transaction: Operation(e.Transaction), Release: e.Release, Environment: e.Environment, Platform: "go", Contexts: contexts, Tags: tags, Spans: e.Spans}
}

func environment() string {
	v := os.Getenv("PB_SENTRY_ENVIRONMENT")
	if v == "" {
		return "production"
	}
	if safeRelease(v) {
		return v
	}
	return "unknown"
}
func safeTraceContext(in map[string]sentry.Context) map[string]sentry.Context {
	trace := in["trace"]
	id, ok := trace["trace_id"].(sentry.TraceID)
	span, okSpan := trace["span_id"].(sentry.SpanID)
	if !ok || !okSpan {
		return nil
	}
	return map[string]sentry.Context{"trace": {"trace_id": id, "span_id": span}}
}

func operationCode(outcome string) string {
	switch outcome {
	case "success":
		return "operation_succeeded"
	case "failed":
		return "operation_failed"
	case "rejected":
		return "operation_rejected"
	case "canceled":
		return "operation_canceled"
	}
	return "state_changed"
}

func safeSpanStatus(value any) sentry.SpanStatus {
	switch value {
	case sentry.SpanStatusOK, "ok":
		return sentry.SpanStatusOK
	case sentry.SpanStatusCanceled, "cancelled":
		return sentry.SpanStatusCanceled
	case sentry.SpanStatusPermissionDenied, "permission_denied":
		return sentry.SpanStatusPermissionDenied
	case sentry.SpanStatusInternalError, "internal_error":
		return sentry.SpanStatusInternalError
	}
	return sentry.SpanStatusUndefined
}
func lifecycleCode(code string) string {
	switch code {
	case "ready", "stopped", "recovered", "approval_pending", "identity_renewed", "start_failed", "start_canceled", "start_deadline", "shutdown_failed", "shutdown_canceled", "shutdown_deadline", "rollback_failed", "rollback_canceled", "rollback_deadline", "route_state", "route_stale":
		return code
	}
	return "lifecycle_state"
}
func (r *Reporter) Lifecycle(ctx context.Context, dimension, name, code, outcome string) {
	ctx = context.WithValue(ctx, lifecycleDimensionKey{}, lifecycleDimension(dimension))
	operation := Operation(name)
	if operation == "command" {
		operation = "runtime_lifecycle"
	}
	code = lifecycleCode(code)
	ctx = context.WithValue(ctx, lifecycleCodeKey{}, code)
	r.Observe(ctx, "paperboat-daemon", operation, outcome, -1)
	if outcome == "failed" {
		r.Capture(ctx, "paperboat-daemon", code)
	}
}

type lifecycleCodeKey struct{}

type lifecycleDimensionKey struct{}

func lifecycleDimension(value string) string {
	switch value {
	case "service", "edge", "config", "route", "origin", "dns", "certificate", "access", "update":
		return value
	}
	return "unknown"
}

type lifecycleComponentKey struct{}

// ServiceComponent returns a finite, payload-free runtime component label.
func ServiceComponent(s string) string { return serviceComponent(s) }

func serviceComponent(s string) string {
	switch s {
	case "storage", "file_transfer_cleanup", "inspector_retention", "authorization", "preview_recovery", "tunnel_enrollment", "peer_transport", "runtime_observation", "edge", "control_plane", "sessions", "config_sync", "protocol", "managed_ssh_authority", "tunnel_manager", "worker_lifecycle", "runtime_attachment":
		return s
	}
	return "unknown"
}
func (r *Reporter) ServiceLifecycle(ctx context.Context, component, stage string, err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx = context.WithValue(ctx, lifecycleComponentKey{}, ServiceComponent(component))
	prefix := "start"
	if stage == "component_shutdown" {
		prefix = "shutdown"
	} else if stage == "component_rollback" {
		prefix = "rollback"
	}
	code, outcome := "ready", "success"
	if prefix != "start" {
		code = "stopped"
	}
	if err != nil {
		fault := ProjectFault(ctx, "paperboat-daemon", stage, stage, prefix+"_failed", err)
		if r != nil && supportref.FromContext(ctx) == "" {
			if reference := r.ambientReference.Load(); reference != nil {
				fault.SupportReference = *reference
			}
		}
		switch fault.Cause {
		case "context_canceled":
			fault.Code = prefix + "_canceled"
		case "deadline_exceeded":
			fault.Code = prefix + "_deadline"
		}
		r.captureFault(ctx, fault, true)
		return
	}

	r.Lifecycle(ctx, "service", stage, code, outcome)
}
