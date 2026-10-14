package supportreport

import (
	"context"
	"math"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/getsentry/sentry-go"
	"github.com/getsentry/sentry-go/attribute"
	serverdiagnostics "github.com/pinksaucepasta/paperboat-server/internal/diagnostics"
	"github.com/pinksaucepasta/paperboat-server/internal/telemetry"
	"github.com/pinksaucepasta/paperboat-server/internal/workers"
)

// Only operational metadata crosses this boundary. In particular, it has no
// field for a URL, request, raw error, message, account, command or payload.
type signalState struct {
	logs, metrics    bool
	sampleRate       float64
	mu               sync.Mutex
	window           time.Time
	counts           [4]int
	dropped          [4]atomic.Uint64
	metricRules      map[string]telemetry.MetricDescriptor
	snapshot         func()
	snapshotMu       sync.Mutex
	collecting       bool
	snapshotPoints   map[string]snapshotGauge
	snapshotCursor   int
	snapshotDeferred atomic.Uint64
}

type signalContextKey struct{}
type signalContext struct {
	service   *Service
	reference string
}

var traceHeaderPattern = regexp.MustCompile(`^[0-9a-f]{32}-[0-9a-f]{16}(-[01])?$`)
var operations = strings.Fields(`http_auth http_accounts http_teams http_billing http_admin http_machines http_terminal http_environment http_config http_files http_tunnels http_previews http_releases http_edge http_network http_support http_other http_health provider_workos provider_stripe provider_github provider_provider_route provider_helper provider_unknown dns_verification certificate_lifecycle certificate_expiry preview_allocation preview_cleanup connector_session connector_connection connector_reconnect connector_backoff connector_handshake connector_disconnect config_apply route_error route_latency service_lifecycle service_watchdog service_crash_loop http_request health_transition worker_operation backup_operation database_operation worker_machines worker_config_warnings worker_config_revocation worker_preview_leases worker_tunnel_expiry worker_connector_rotation worker_connector_drain worker_domain_dns worker_preview_dns worker_preview_outbox worker_team_retention worker_team_entitlements worker_team_billing worker_team_inbox worker_edge_assignments worker_stale_nodes worker_edge_dns worker_diagnostic_uploads worker_certificates worker_platform_certificates worker_reporting_cleanup worker_reporting_storage`)

func validOperation(v string) bool { return slices.Contains(operations, v) }
func validOutcome(v string) bool {
	return slices.Contains([]string{"success", "failed", "rejected", "canceled", "state_change"}, v)
}

func (s *Service) configureSignals(cfg Config, options *sentry.ClientOptions) error {
	rate := .1
	if cfg.TracesSampleRate != nil {
		rate = *cfg.TracesSampleRate
	}
	if !(rate >= 0 && rate <= 1) {
		return ErrInvalid
	}
	state := &signalState{logs: cfg.LogsEnabled == nil || *cfg.LogsEnabled, metrics: cfg.MetricsEnabled == nil || *cfg.MetricsEnabled, sampleRate: rate, metricRules: make(map[string]telemetry.MetricDescriptor)}
	for _, d := range telemetry.MetricDescriptors() {
		switch d.Kind {
		case telemetry.MetricCounter:
			state.metricRules[d.Name+"_snapshot"] = d
		case telemetry.MetricHistogram:
			state.metricRules[d.Name+"_sum_snapshot"] = d
			state.metricRules[d.Name+"_count_snapshot"] = d
		default:
			state.metricRules[d.Name] = d
		}
	}
	for _, name := range cfg.PlatformMetricNames {
		if tokenPattern.MatchString(name) {
			state.metricRules["paperboat_server_"+name+"_snapshot"] = telemetry.MetricDescriptor{}
		}
	}
	for _, name := range strings.Fields("paperboat.backup.state paperboat.restore.state paperboat.backup.age_seconds paperboat.restore.age_seconds paperboat.database.connections paperboat.database.acquired paperboat.database.idle paperboat.database.acquire_wait_seconds_snapshot paperboat.database.acquire_canceled_snapshot paperboat.runtime.goroutines paperboat.runtime.heap_bytes paperboat.reporting.dropped paperboat.reporting.cleanup_failures") {
		state.metricRules[name] = telemetry.MetricDescriptor{}
	}
	for _, name := range []string{"paperboat.worker.state", "paperboat.worker.progress_age_seconds"} {
		state.metricRules[name] = telemetry.MetricDescriptor{Labels: []telemetry.LabelDescriptor{{Name: "worker", AllowedValues: workers.ProgressNames()}}}
	}
	s.signals = state
	options.EnableTracing = rate > 0
	options.TracesSampleRate = rate
	// Incoming sampling is not authority to force an unbounded sample rate.
	options.TracesSampler = func(sentry.SamplingContext) float64 { return rate }
	options.MaxSpans = 64
	options.BeforeSendLog = s.cleanLog
	options.BeforeSendMetric = s.cleanMetric
	options.BeforeSendTransaction = s.cleanTransaction
	return nil
}

func (s *Service) admitSignal(kind int) bool {
	if s == nil || s.client == nil || s.signals == nil || s.closed.Load() {
		return false
	}
	st := s.signals
	st.mu.Lock()
	defer st.mu.Unlock()
	now := time.Now()
	if now.Sub(st.window) >= time.Minute {
		st.window = now
		st.counts = [4]int{}
	}
	if st.counts[kind] >= [4]int{600, 300, 6000, 600}[kind] {
		st.dropped[kind].Add(1)
		s.sdkSubmissionsDropped.Add(1)
		return false
	}
	st.counts[kind]++
	return true
}

func (s *Service) signalContext(ctx context.Context, reference string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if v, ok := ctx.Value(signalContextKey{}).(signalContext); ok && v.service == s {
		if reference == "" || reference == v.reference {
			return ctx
		}
	}
	if !ValidReference(reference) {
		reference = ""
	}
	scope := sentry.NewScope()
	scope.SetTag("component", "paperboat-server")
	if reference != "" {
		scope.SetTag("support_reference", reference)
	}
	ctx = sentry.SetHubOnContext(ctx, sentry.NewHub(s.client, scope))
	ctx = context.WithValue(ctx, signalContextKey{}, signalContext{s, reference})
	return serverdiagnostics.WithProviderObserver(ctx, BeginDependency)
}

// Begin records one owned operation. The returned finish is idempotent. Tracing
// headers are accepted only by callers at Paperboat control API boundaries.
func (s *Service) Begin(ctx context.Context, operation, reference, parent string) (context.Context, func(string)) {
	if s == nil || !s.beginAdmission() {
		return ctx, func(string) {}
	}
	defer s.endAdmission()
	ctx = s.Context(ctx, reference)
	if s.client == nil || !validOperation(operation) {
		return ctx, func(string) {}
	}
	ctx = s.signalContext(ctx, reference)
	sc, _ := ctx.Value(signalContextKey{}).(signalContext)
	started := time.Now()
	var span *sentry.Span
	if s.signals.sampleRate > 0 && s.admitSignal(1) {
		options := []sentry.SpanOption{sentry.WithTransactionName(operation)}
		if sentry.SpanFromContext(ctx) == nil && traceHeaderPattern.MatchString(parent) {
			options = append(options, sentry.ContinueTrace(sentry.GetHubFromContext(ctx), parent, ""))
		}
		span = sentry.StartSpan(ctx, operation, options...)
		span.SetTag("component", "paperboat-server")
		span.SetTag("operation", operation)
		if sc.reference != "" {
			span.SetTag("support_reference", sc.reference)
		}
		ctx = span.Context()
	}
	var once sync.Once
	return ctx, func(outcome string) {
		once.Do(func() {
			if !s.beginAdmission() {
				return
			}
			defer s.endAdmission()
			if !validOutcome(outcome) {
				outcome = "failed"
			}
			s.operationAdmitted(ctx, operation, outcome, sc.reference, time.Since(started))
			if span != nil {
				span.SetTag("outcome", outcome)
				span.Status = sentry.SpanStatusOK
				if outcome == "failed" {
					span.Status = sentry.SpanStatusInternalError
				}
				if outcome == "canceled" {
					span.Status = sentry.SpanStatusCanceled
				}
				if outcome == "rejected" {
					span.Status = sentry.SpanStatusPermissionDenied
				}
				span.Finish()
			}
		})
	}
}

// BeginDependency is a no-op unless a configured reporter is on the context.
// It never propagates diagnostic headers to external providers.
func BeginDependency(ctx context.Context, provider string) (context.Context, func(string)) {
	sc, _ := ctx.Value(signalContextKey{}).(signalContext)
	if sc.service == nil {
		return ctx, func(string) {}
	}
	return sc.service.Begin(ctx, "provider_"+provider, sc.reference, "")
}

func (s *Service) Operation(ctx context.Context, operation, outcome, reference string, duration time.Duration) {
	if s == nil || s.client == nil || s.signals == nil || !validOperation(operation) || !validOutcome(outcome) || !s.beginAdmission() {
		return
	}
	defer s.endAdmission()
	s.operationAdmitted(ctx, operation, outcome, reference, duration)
}

func (s *Service) operationAdmitted(ctx context.Context, operation, outcome, reference string, duration time.Duration) {
	ctx = s.signalContext(ctx, reference)
	if s.signals.logs && s.admitSignal(0) {
		logger := sentry.NewLogger(ctx)
		entry := logger.Info()
		if outcome == "failed" {
			entry = logger.Error()
		} else if outcome == "rejected" {
			entry = logger.Warn()
		}
		entry.String("component", "paperboat-server").String("operation", operation).String("outcome", outcome).String("support_reference", reference).Emit("paperboat.operation")
	}
	if s.signals.metrics && s.admitSignal(3) {
		meter := sentry.NewMeter(ctx)
		attrs := sentry.WithAttributes(attribute.String("component", "paperboat-server"), attribute.String("operation", operation), attribute.String("outcome", outcome))
		meter.Count("paperboat.operation.count", 1, attrs)
		if duration >= 0 {
			meter.Distribution("paperboat.operation.duration", duration.Seconds(), attrs, sentry.WithUnit(sentry.UnitSecond))
		}
	}
}

func (s *Service) ObserveEvent(event telemetry.Event) {
	if !validOperation(event.Name) || !validOutcome(string(event.Outcome)) {
		return
	}
	// Event.Message, all resource IDs and user-provided diagnostic fields stay local.
	s.Operation(context.Background(), event.Name, string(event.Outcome), event.CorrelationID, -1)
}

// SetSnapshot is called once during composition, before Run starts. The source
// uses existing bounded registries and never polls the database or a provider.
func (s *Service) SetSnapshot(fn func()) {
	if s != nil && s.signals != nil {
		s.signals.snapshot = fn
	}
}

const snapshotBatchLimit = 100
const snapshotCandidateLimit = 2048
const snapshotInterval = 5 * time.Second

type snapshotGauge struct {
	name  string
	value float64
	attrs []attribute.Builder
}

func (s *Service) snapshotMetrics() {
	if s == nil || s.signals == nil || !s.signals.metrics || s.client == nil || s.signals.snapshot == nil {
		return
	}
	st := s.signals
	st.snapshotMu.Lock()
	defer st.snapshotMu.Unlock()
	st.mu.Lock()
	st.collecting = true
	st.snapshotPoints = make(map[string]snapshotGauge)
	st.mu.Unlock()
	st.snapshot()
	s.Gauge("paperboat.reporting.dropped", float64(s.Dropped()+st.dropped[0].Load()+st.dropped[1].Load()+st.dropped[2].Load()+st.dropped[3].Load()))
	s.Gauge("paperboat.reporting.cleanup_failures", float64(s.CleanupFailures()))
	st.mu.Lock()
	points := st.snapshotPoints
	st.snapshotPoints = nil
	st.collecting = false
	st.mu.Unlock()
	keys := make([]string, 0, len(points))
	for key := range points {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	count := len(keys)
	if count == 0 {
		return
	}
	limit := min(count, snapshotBatchLimit)
	st.snapshotDeferred.Add(uint64(count - limit))
	start := st.snapshotCursor % count
	st.snapshotCursor = (start + limit) % count
	for i := 0; i < limit; i++ {
		point := points[keys[(start+i)%count]]
		s.emitGauge(point.name, point.value, point.attrs)
	}
}

func (s *Service) RegistrySnapshot(samples []telemetry.MetricSample) {
	for _, v := range samples {
		attrs := make([]attribute.Builder, 0, len(v.Labels)+1)
		attrs = append(attrs, attribute.String("component", "paperboat-server"))
		for _, l := range v.Labels {
			attrs = append(attrs, attribute.String(l.Name, l.Value))
		}
		switch v.Kind {
		case telemetry.MetricCounter:
			s.gauge(v.Name+"_snapshot", float64(v.Value), attrs)
		case telemetry.MetricHistogram:
			s.gauge(v.Name+"_sum_snapshot", v.Sum, attrs)
			s.gauge(v.Name+"_count_snapshot", float64(v.Count), attrs)
		default:
			s.gauge(v.Name, float64(v.Value), attrs)
		}
	}
}
func (s *Service) PlatformSnapshot(values map[string]int64) {
	for name, value := range values {
		s.Gauge("paperboat_server_"+name+"_snapshot", float64(value))
	}
}
func (s *Service) Gauge(name string, value float64) {
	s.gauge(name, value, []attribute.Builder{attribute.String("component", "paperboat-server")})
}
func (s *Service) gauge(name string, value float64, attrs []attribute.Builder) {
	if s == nil || s.signals == nil || !s.signals.metrics || s.client == nil || s.closed.Load() {
		return
	}
	input := map[string]attribute.Value{}
	for _, attr := range attrs {
		input[attr.Key] = attr.Value
	}
	clean := s.cleanMetric(&sentry.Metric{Name: name, Value: sentry.Float64MetricValue(value), Attributes: input})
	if clean == nil {
		return
	}
	keys := make([]string, 0, len(clean.Attributes))
	for key := range clean.Attributes {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	attrs = make([]attribute.Builder, 0, len(keys))
	var identity strings.Builder
	identity.WriteString(name)
	for _, key := range keys {
		v := clean.Attributes[key]
		attrs = append(attrs, attribute.Builder{Key: key, Value: v})
		identity.WriteByte('|')
		identity.WriteString(key)
		identity.WriteByte('=')
		identity.WriteString(v.AsString())
	}
	st := s.signals
	st.mu.Lock()
	if st.collecting {
		key := identity.String()
		if _, exists := st.snapshotPoints[key]; exists || len(st.snapshotPoints) < snapshotCandidateLimit {
			st.snapshotPoints[key] = snapshotGauge{name, value, attrs}
		} else {
			st.dropped[2].Add(1)
		}
		st.mu.Unlock()
		return
	}
	st.mu.Unlock()
	s.emitGauge(name, value, attrs)
}
func (s *Service) emitGauge(name string, value float64, attrs []attribute.Builder) {
	if !s.beginAdmission() {
		return
	}
	defer s.endAdmission()
	if !s.admitSignal(2) {
		return
	}
	sentry.NewMeter(s.signalContext(context.Background(), "")).Gauge(name, value, sentry.WithAttributes(attrs...))
}

func (s *Service) SignalStatus() map[string]any {
	if s == nil || s.signals == nil || s.client == nil {
		if s == nil {
			return map[string]any{"enabled": false}
		}
		return map[string]any{"enabled": false, "cleanup_failures": s.CleanupFailures(), "storage_failures": s.StoreFailures(), "dropped_records": s.Dropped(), "sdk_http_failures": s.SDKHTTPFailures(), "sdk_submissions_dropped": s.SDKSubmissionsDropped(), "flush_status": s.FlushStatus(), "delivery": "unconfirmed"}
	}
	return map[string]any{"enabled": true, "logs_enabled": s.signals.logs, "metrics_enabled": s.signals.metrics, "traces_sample_rate": s.signals.sampleRate, "logs_dropped": s.signals.dropped[0].Load(), "traces_dropped": s.signals.dropped[1].Load(), "metrics_dropped": s.signals.dropped[2].Load() + s.signals.dropped[3].Load(), "snapshot_deferred": s.signals.snapshotDeferred.Load(), "snapshot_interval_seconds": int(snapshotInterval / time.Second), "snapshot_batch_limit": snapshotBatchLimit, "cleanup_failures": s.CleanupFailures(), "storage_failures": s.StoreFailures(), "dropped_records": s.Dropped(), "sdk_http_failures": s.SDKHTTPFailures(), "sdk_submissions_dropped": s.SDKSubmissionsDropped(), "flush_status": s.FlushStatus(), "delivery": "unconfirmed"}
}

func (s *Service) cleanLog(l *sentry.Log) *sentry.Log {
	if l == nil || !s.signals.logs || l.Body != "paperboat.operation" {
		return nil
	}
	op, outcome := l.Attributes["operation"].AsString(), l.Attributes["outcome"].AsString()
	if !validOperation(op) || !validOutcome(outcome) {
		return nil
	}
	attrs := map[string]attribute.Value{"component": attribute.StringValue("paperboat-server"), "operation": attribute.StringValue(op), "outcome": attribute.StringValue(outcome), "release": attribute.StringValue(s.release)}
	s.signalIdentity(attrs)
	s.fleetAttributes(attrs)
	ref := l.Attributes["support_reference"].AsString()
	if ValidReference(ref) {
		attrs["support_reference"] = attribute.StringValue(ref)
	}
	level, severity := sentry.LogLevelInfo, 9
	if outcome == "failed" {
		level, severity = sentry.LogLevelError, 17
	} else if outcome == "rejected" {
		level, severity = sentry.LogLevelWarn, 13
	}
	return &sentry.Log{Timestamp: l.Timestamp, TraceID: l.TraceID, SpanID: l.SpanID, Level: level, Severity: severity, Body: "paperboat.operation", Attributes: attrs}
}
func (s *Service) cleanMetric(m *sentry.Metric) *sentry.Metric {
	if m == nil || !s.signals.metrics {
		return nil
	}
	switch value := m.Value.AsInterface().(type) {
	case int64:
		if value < 0 {
			return nil
		}
	case float64:
		if value < 0 || math.IsNaN(value) || math.IsInf(value, 0) {
			return nil
		}
	default:
		return nil
	}
	if m.Name == "paperboat.worker.state" {
		var state float64
		switch value := m.Value.AsInterface().(type) {
		case int64:
			state = float64(value)
		case float64:
			state = value
		}
		if state > 5 || math.Trunc(state) != state {
			return nil
		}
	}
	if m.Name == "paperboat.backup.state" || m.Name == "paperboat.restore.state" {
		var state float64
		switch value := m.Value.AsInterface().(type) {
		case int64:
			state = float64(value)
		case float64:
			state = value
		}
		if state > 6 || math.Trunc(state) != state {
			return nil
		}
	}
	attrs := map[string]attribute.Value{"component": attribute.StringValue("paperboat-server")}
	if m.Name == "paperboat.operation.count" || m.Name == "paperboat.operation.duration" {
		op, outcome := m.Attributes["operation"].AsString(), m.Attributes["outcome"].AsString()
		if !validOperation(op) || !validOutcome(outcome) {
			return nil
		}
		attrs["operation"], attrs["outcome"] = attribute.StringValue(op), attribute.StringValue(outcome)
	} else {
		rule, ok := s.signals.metricRules[m.Name]
		if !ok {
			return nil
		}
		for _, label := range rule.Labels {
			v := m.Attributes[label.Name].AsString()
			if !slices.Contains(label.AllowedValues, v) {
				return nil
			}
			attrs[label.Name] = attribute.StringValue(v)
		}
	}
	s.fleetAttributes(attrs)
	s.signalIdentity(attrs)
	// High-cardinality identifiers are not even retained as metric trace linkage.
	kind, unit := sentry.MetricTypeGauge, ""
	if m.Name == "paperboat.operation.count" {
		kind = sentry.MetricTypeCounter
	}
	if m.Name == "paperboat.operation.duration" {
		kind = sentry.MetricTypeDistribution
		unit = sentry.UnitSecond
	}
	return &sentry.Metric{Timestamp: m.Timestamp, Type: kind, Name: m.Name, Value: m.Value, Unit: unit, Attributes: attrs}
}
func safeTraceContext(c sentry.Context) sentry.Context {
	out := sentry.Context{}
	for _, name := range []string{"trace_id", "span_id", "parent_span_id"} {
		switch v := c[name].(type) {
		case sentry.TraceID:
			out[name] = v
		case sentry.SpanID:
			out[name] = v
		}
	}
	if op, ok := c["op"].(string); ok && validOperation(op) {
		out["op"] = op
	}
	if status, ok := c["status"].(sentry.SpanStatus); ok && status >= sentry.SpanStatusOK && status <= sentry.SpanStatusUnauthenticated {
		out["status"] = status
	}
	return out
}
func safeSignalTags(tags map[string]string) map[string]string {
	clean := map[string]string{"component": "paperboat-server"}
	if validOperation(tags["operation"]) {
		clean["operation"] = tags["operation"]
	}
	if validOutcome(tags["outcome"]) {
		clean["outcome"] = tags["outcome"]
	}
	if ValidReference(tags["support_reference"]) {
		clean["support_reference"] = tags["support_reference"]
	}
	return clean
}
func (s *Service) cleanTransaction(e *sentry.Event, _ *sentry.EventHint) *sentry.Event {
	if e == nil || !validOperation(e.Transaction) {
		return nil
	}
	out := &sentry.Event{EventID: e.EventID, Type: "transaction", Platform: "go", Release: s.release, Environment: e.Environment, Transaction: e.Transaction, StartTime: e.StartTime, Timestamp: e.Timestamp, Tags: safeSignalTags(e.Tags), Contexts: map[string]sentry.Context{"trace": safeTraceContext(e.Contexts["trace"])}}
	for _, span := range e.Spans {
		if span == nil || !validOperation(span.Op) || len(out.Spans) >= 64 {
			continue
		}
		out.Spans = append(out.Spans, &sentry.Span{TraceID: span.TraceID, SpanID: span.SpanID, ParentSpanID: span.ParentSpanID, Op: span.Op, StartTime: span.StartTime, EndTime: span.EndTime, Status: span.Status, Tags: safeSignalTags(span.Tags)})
	}
	s.fleetTags(out.Tags)
	for _, span := range out.Spans {
		s.fleetTags(span.Tags)
	}
	return out
}

// Context attaches the configured reporter without keeping a process-lifetime
// transaction alive. Each child job/query can create its own bounded trace.
func (s *Service) Context(ctx context.Context, reference string) context.Context {
	if s == nil || s.closed.Load() {
		return ctx
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if s.client == nil {
		if !ValidReference(reference) {
			reference = ""
		}
		return context.WithValue(ctx, signalContextKey{}, signalContext{service: s, reference: reference})
	}
	return s.signalContext(ctx, reference)
}

// Fleet identity is public deployment metadata, never a node or customer ID.
var regionPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,23}$`)
var instancePattern = regexp.MustCompile(`^slot-(0[1-9]|[1-5][0-9]|6[0-4])$`)

func ValidFleet(region, instance string) bool {
	return region == "" && instance == "" || regionPattern.MatchString(region) && instancePattern.MatchString(instance)
}
func (s *Service) fleetTags(tags map[string]string) {
	if s.region != "" {
		tags["region"], tags["instance"] = s.region, s.instance
	}
}
func (s *Service) fleetAttributes(attrs map[string]attribute.Value) {
	if s.region != "" {
		attrs["region"], attrs["instance"] = attribute.StringValue(s.region), attribute.StringValue(s.instance)
	}
}

// Rebuild SDK identity from validated configuration instead of trusting fields
// attached by a caller or retaining the SDK's host-name defaults.
func (s *Service) signalIdentity(attrs map[string]attribute.Value) {
	if tokenPattern.MatchString(s.release) {
		attrs["sentry.release"] = attribute.StringValue(s.release)
	}
	if tokenPattern.MatchString(s.environment) {
		attrs["sentry.environment"] = attribute.StringValue(s.environment)
	}
}

// WorkerProgress accepts only mounted-worker enum labels; no job resource IDs.
func (s *Service) WorkerProgress(name string, state int, age *float64) {
	attrs := []attribute.Builder{attribute.String("worker", name)}
	s.gauge("paperboat.worker.state", float64(state), attrs)
	if age != nil {
		s.gauge("paperboat.worker.progress_age_seconds", *age, attrs)
	}
}
