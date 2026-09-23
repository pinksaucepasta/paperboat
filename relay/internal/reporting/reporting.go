// Package reporting emits deliberately minimal, privacy-safe service failures.
package reporting

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"math"
	"net/url"
	"os"
	"path"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/getsentry/sentry-go"
	"github.com/getsentry/sentry-go/attribute"
)

const (
	maxEvents        = 8
	maxTraces        = 128
	maxDetailMetrics = 64
	maxSnapshots     = 64
	flushTimeout     = 2 * time.Second
)

// DefaultDSN and DefaultRelease are populated only in official release builds.
// Ordinary source builds leave both empty and therefore keep reporting disabled.
var (
	DefaultDSN     string
	DefaultRelease string
)

type Reporter struct {
	client                *sentry.Client
	hub                   *sentry.Hub
	enabled               bool
	logs, traces, metrics bool
	component             string
	region, instance      string
	sent                  atomic.Uint32
	logsSent              atomic.Uint32
	metricsSent           atomic.Uint32
	snapshotCursor        atomic.Uint64
	tracesSent            atomic.Uint32
	window                atomic.Int64
	droppedLogs           atomic.Uint64
	droppedMetrics        atomic.Uint64
	droppedErrors         atomic.Uint64
	droppedTraces         atomic.Uint64
	closed                atomic.Bool
}

func New(component string) (*Reporter, error) {
	if component != "paperboat-relay" && component != "paperboat-tunnel" {
		return nil, errors.New("unsupported Sentry component")
	}
	enabledText := strings.TrimSpace(os.Getenv("PAPERBOAT_SENTRY_ENABLED"))
	if enabledText != "" {
		enabled, err := strconv.ParseBool(enabledText)
		if err != nil {
			return nil, errors.New("PAPERBOAT_SENTRY_ENABLED must be a boolean")
		}
		if !enabled {
			return &Reporter{}, nil
		}
	}
	dsn := firstNonempty(os.Getenv("PAPERBOAT_SENTRY_DSN"), DefaultDSN)
	release := firstNonempty(os.Getenv("PAPERBOAT_SENTRY_RELEASE"), DefaultRelease)
	if enabledText == "" && strings.TrimSpace(DefaultDSN) == "" {
		return &Reporter{}, nil
	}
	if dsn == "" || release == "" {
		return nil, errors.New("enabled Sentry reporting requires PAPERBOAT_SENTRY_DSN and PAPERBOAT_SENTRY_RELEASE")
	}
	parsedDSN, err := url.Parse(dsn)
	if err != nil || parsedDSN.Scheme != "https" || parsedDSN.Host == "" || parsedDSN.RawQuery != "" || parsedDSN.Fragment != "" {
		return nil, errors.New("PAPERBOAT_SENTRY_DSN must be an HTTPS DSN without query or fragment")
	}
	if !releasePattern.MatchString(release) {
		return nil, errors.New("PAPERBOAT_SENTRY_RELEASE must be a non-empty safe identifier")
	}
	logs, err := optionalBool("PAPERBOAT_SENTRY_LOGS_ENABLED", true)
	if err != nil {
		return nil, err
	}
	metrics, err := optionalBool("PAPERBOAT_SENTRY_METRICS_ENABLED", true)
	if err != nil {
		return nil, err
	}
	traceRate, err := optionalRate("PAPERBOAT_SENTRY_TRACES_SAMPLE_RATE", .1)
	if err != nil {
		return nil, err
	}
	environment := strings.TrimSpace(os.Getenv("PAPERBOAT_SENTRY_ENVIRONMENT"))
	if environment == "" {
		environment = "production"
	}
	if environment != safeToken(environment) {
		return nil, errors.New("PAPERBOAT_SENTRY_ENVIRONMENT must be a safe identifier")
	}
	region, instance, err := fleetEnvironment()
	if err != nil {
		return nil, err
	}
	transport := sentry.NewHTTPTransport()
	transport.BufferSize = maxEvents
	transport.Timeout = flushTimeout
	config := options(component, release, dsn, transport)
	config.BeforeSend = func(event *sentry.Event, _ *sentry.EventHint) *sentry.Event {
		return sanitizeEnvironment(event, component, release, environment, region, instance)
	}
	config.BeforeSendLog = func(log *sentry.Log) *sentry.Log {
		return sanitizeLog(log, component, release, environment, region, instance)
	}
	config.BeforeSendMetric = func(metric *sentry.Metric) *sentry.Metric {
		return sanitizeMetric(metric, component, release, environment, region, instance)
	}
	config.BeforeSendTransaction = func(event *sentry.Event, _ *sentry.EventHint) *sentry.Event {
		return sanitizeTransaction(event, component, release, environment, region, instance)
	}
	config.EnableTracing = traceRate > 0
	config.TracesSampleRate = traceRate
	config.Environment = environment
	config.PropagateTraceparent = false
	config.TracePropagationTargets = []string{}
	client, err := sentry.NewClient(config)
	if err != nil {
		return nil, errors.New("cannot configure Sentry reporting")
	}
	return &Reporter{client: client, hub: sentry.NewHub(client, sentry.NewScope()), enabled: true, logs: logs, traces: traceRate > 0, metrics: metrics, component: component, region: region, instance: instance}, nil
}

func firstNonempty(value, fallback string) string {
	if value = strings.TrimSpace(value); value != "" {
		return value
	}
	return strings.TrimSpace(fallback)
}

var regionPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,23}$`)
var instancePattern = regexp.MustCompile(`^slot-(0[1-9]|[1-5][0-9]|6[0-4])$`)
var releasePattern = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,128}$`)

func fleetEnvironment() (string, string, error) {
	region, instance := strings.TrimSpace(os.Getenv("PAPERBOAT_SENTRY_REGION")), strings.TrimSpace(os.Getenv("PAPERBOAT_SENTRY_INSTANCE"))
	if region == "" && instance == "" {
		return "", "", nil
	}
	if !regionPattern.MatchString(region) || !instancePattern.MatchString(instance) {
		return "", "", errors.New("PAPERBOAT_SENTRY_REGION and PAPERBOAT_SENTRY_INSTANCE must be supplied together with valid hosted deployment values")
	}
	return region, instance, nil
}

func optionalBool(name string, fallback bool) (bool, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.ParseBool(raw)
	if err != nil {
		return false, errors.New(name + " must be a boolean")
	}
	return value, nil
}
func addFleetAttributes(attrs map[string]attribute.Value, fleet []string) {
	if len(fleet) == 2 && fleet[0] != "" && fleet[1] != "" {
		attrs["region"] = attribute.StringValue(fleet[0])
		attrs["instance"] = attribute.StringValue(fleet[1])
	}
}
func addFleetTags(tags map[string]string, fleet []string) {
	if len(fleet) == 2 && fleet[0] != "" && fleet[1] != "" {
		tags["region"], tags["instance"] = fleet[0], fleet[1]
	}
}
func optionalRate(name string, fallback float64) (float64, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil || value < 0 || value > 1 {
		return 0, errors.New(name + " must be between 0 and 1")
	}
	return value, nil
}

func options(component, release, dsn string, transport sentry.Transport) sentry.ClientOptions {
	return sentry.ClientOptions{
		Dsn: dsn, Release: release, Transport: transport,
		AttachStacktrace: false, EnableTracing: false, TracesSampleRate: 0,
		SendDefaultPII: false, MaxBreadcrumbs: 0, DataCollection: &sentry.DataCollection{UserInfo: sentry.Set(false), Cookies: &sentry.KeyValueCollectionBehavior{Mode: sentry.CollectionOff}, HTTPHeaders: &sentry.HeaderCollectionConfig{Request: &sentry.KeyValueCollectionBehavior{Mode: sentry.CollectionOff}, Response: &sentry.KeyValueCollectionBehavior{Mode: sentry.CollectionOff}}, HTTPBodies: []sentry.BodyType{}, QueryParams: &sentry.KeyValueCollectionBehavior{Mode: sentry.CollectionOff}},
		Integrations: func([]sentry.Integration) []sentry.Integration { return nil },
		BeforeSend: func(event *sentry.Event, _ *sentry.EventHint) *sentry.Event {
			return sanitize(event, component, release)
		},
		BeforeSendLog:    func(log *sentry.Log) *sentry.Log { return sanitizeLog(log, component, release, "") },
		BeforeSendMetric: func(metric *sentry.Metric) *sentry.Metric { return sanitizeMetric(metric, component, release, "") },
		BeforeSendTransaction: func(event *sentry.Event, _ *sentry.EventHint) *sentry.Event {
			return sanitizeTransaction(event, component, release, "")
		},
	}
}

func sanitizeLog(log *sentry.Log, component, release, environment string, fleet ...string) *sentry.Log {
	if log == nil {
		return nil
	}
	operation := attributeString(log.Attributes, "operation")
	outcome := attributeString(log.Attributes, "outcome")
	code := attributeString(log.Attributes, "code")
	reference := attributeString(log.Attributes, "support_reference")
	if !operations[operation] || !outcomes[outcome] || !codes[code] || (reference != "" && !validReference(reference)) {
		return nil
	}
	level, severity := sentry.LogLevelInfo, sentry.LogSeverityInfo
	if outcome == "failed" {
		level, severity = sentry.LogLevelError, sentry.LogSeverityError
	}
	attrs := map[string]attribute.Value{"component": attribute.StringValue(component), "sentry.release": attribute.StringValue(release), "sentry.environment": attribute.StringValue(environment), "operation": attribute.StringValue(operation), "outcome": attribute.StringValue(outcome), "code": attribute.StringValue(code), "support_reference": attribute.StringValue(reference)}
	addFleetAttributes(attrs, fleet)
	return &sentry.Log{Timestamp: log.Timestamp, TraceID: log.TraceID, SpanID: log.SpanID, Level: level, Severity: severity, Body: "paperboat.operation", Attributes: attrs}
}
func sanitizeMetric(metric *sentry.Metric, component, release, environment string, fleet ...string) *sentry.Metric {
	if metric == nil {
		return nil
	}
	switch value := metric.Value.AsInterface().(type) {
	case int64:
	case float64:
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return nil
		}
	default:
		return nil
	}
	spec, ok := metricCatalog[metric.Name]
	if !ok || metric.Type != spec.kind {
		return nil
	}
	attrs := map[string]attribute.Value{"component": attribute.StringValue(component), "sentry.release": attribute.StringValue(release), "sentry.environment": attribute.StringValue(environment)}
	addFleetAttributes(attrs, fleet)
	seen := map[string]bool{}
	for key, value := range metric.Attributes {
		if key == "component" || key == "region" || key == "instance" || strings.HasPrefix(key, "sentry.") {
			continue
		}
		allowed, ok := spec.labels[key]
		if !ok {
			return nil
		}
		text, ok := value.AsInterface().(string)
		if !ok || !allowed[text] || seen[key] {
			return nil
		}
		seen[key] = true
		attrs[key] = attribute.StringValue(text)
	}
	if len(seen) != len(spec.labels) {
		return nil
	}
	copy := *metric
	copy.Attributes = attrs
	copy.Unit = ""
	if metric.Name == "paperboat.operation.duration" {
		copy.Unit = sentry.UnitSecond
	}
	copy.TraceID = sentry.TraceID{}
	copy.SpanID = sentry.SpanID{}
	return &copy
}
func sanitizeTransaction(event *sentry.Event, component, release, environment string, fleet ...string) *sentry.Event {
	if event == nil || !operations[event.Transaction] {
		return nil
	}
	tags := map[string]string{"component": component, "operation": event.Transaction}
	addFleetTags(tags, fleet)
	for _, key := range []string{"outcome", "code"} {
		value := event.Tags[key]
		if key == "outcome" && !outcomes[value] || key == "code" && !codes[value] {
			return nil
		}
		tags[key] = value
	}
	if reference := event.Tags["support_reference"]; reference != "" {
		if !validReference(reference) {
			return nil
		}
		tags["support_reference"] = reference
	}
	contexts := map[string]sentry.Context{}
	if trace, ok := event.Contexts["trace"]; ok {
		clean := sentry.Context{"op": "paperboat." + event.Transaction}
		if tags["outcome"] == "failed" || tags["outcome"] == "rejected" {
			clean["status"] = "internal_error"
		} else {
			clean["status"] = "ok"
		}
		if value, ok := trace["trace_id"].(sentry.TraceID); ok && value != (sentry.TraceID{}) {
			clean["trace_id"] = value
		}
		if value, ok := trace["span_id"].(sentry.SpanID); ok && value != (sentry.SpanID{}) {
			clean["span_id"] = value
		}
		if value, ok := trace["parent_span_id"].(sentry.SpanID); ok && value != (sentry.SpanID{}) {
			clean["parent_span_id"] = value
		}
		contexts["trace"] = clean
	}
	return &sentry.Event{EventID: event.EventID, Timestamp: event.Timestamp, StartTime: event.StartTime, Type: "transaction", Transaction: event.Transaction, TransactionInfo: &sentry.TransactionInfo{Source: sentry.SourceCustom}, Platform: "go", Release: release, Environment: environment, Tags: tags, Contexts: contexts}
}
func attributeString(values map[string]attribute.Value, key string) string {
	value, ok := values[key]
	if !ok {
		return ""
	}
	text, _ := value.AsInterface().(string)
	return text
}
func validHex(value string, size int) bool {
	if len(value) != size {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil && strings.Trim(value, "0") != "" && value == strings.ToLower(value)
}

type metricSpec struct {
	kind   sentry.MetricType
	labels map[string]map[string]bool
}

func values(items ...string) map[string]bool {
	out := map[string]bool{}
	for _, item := range items {
		out[item] = true
	}
	return out
}

var operationLabels = map[string]map[string]bool{"operation": operations, "outcome": outcomes, "code": codes}
var metricCatalog = map[string]metricSpec{
	"paperboat.operation.count": {sentry.MetricTypeCounter, operationLabels}, "paperboat.operation.duration": {sentry.MetricTypeDistribution, operationLabels},
	"paperboat_relay_sessions": {sentry.MetricTypeGauge, nil}, "paperboat_relay_capacity_limit": {sentry.MetricTypeGauge, nil},
	"paperboat_relay_admissions_total_snapshot": {sentry.MetricTypeGauge, map[string]map[string]bool{"outcome": values("success", "rejected")}}, "paperboat_relay_packets_total_snapshot": {sentry.MetricTypeGauge, map[string]map[string]bool{"outcome": values("forwarded", "dropped")}},
	"paperboat_peer_relay_allocations": {sentry.MetricTypeGauge, nil}, "paperboat_peer_relay_admissions_total_snapshot": {sentry.MetricTypeGauge, map[string]map[string]bool{"outcome": values("success", "rejected")}}, "paperboat_peer_relay_packets_total_snapshot": {sentry.MetricTypeGauge, nil},
	"paperboat_reporting_errors_dropped_total_snapshot": {sentry.MetricTypeGauge, nil}, "paperboat_reporting_logs_dropped_total_snapshot": {sentry.MetricTypeGauge, nil}, "paperboat_reporting_metrics_dropped_total_snapshot": {sentry.MetricTypeGauge, nil}, "paperboat_reporting_traces_dropped_total_snapshot": {sentry.MetricTypeGauge, nil},
	"paperboat_reporting_signal_enabled": {sentry.MetricTypeGauge, map[string]map[string]bool{"signal": values("errors", "logs", "metrics", "traces")}},
}

var operations = map[string]bool{"relay_admission": true, "relay_session": true, "relay_service": true, "service_lifecycle": true, "dependency_health": true, "control_reconcile": true, "connector_admission": true, "connector_attach": true, "connector_stream": true, "usage_delivery": true, "capacity": true, "backlog": true}
var outcomes = map[string]bool{"success": true, "failed": true, "rejected": true, "canceled": true, "state_change": true}
var codes = map[string]bool{"ok": true, "internal": true, "invalid": true, "unauthorized": true, "capacity": true, "unavailable": true, "timeout": true, "shutdown": true, "recovered": true}

// Observe emits a bounded fixed-cardinality operation log, trace, and counter.
func (r *Reporter) Observe(ctx context.Context, operation, outcome, code, reference string, duration time.Duration) {
	if r == nil || !r.enabled || r.closed.Load() || !operations[operation] || !outcomes[outcome] || !codes[code] {
		return
	}
	if reference != "" && !validReference(reference) {
		return
	}
	ctx = sentry.SetHubOnContext(ctx, r.hub)
	if r.traces && r.permit(&r.tracesSent, &r.droppedTraces, maxTraces) {
		span := sentry.StartSpan(ctx, "paperboat."+operation, sentry.WithTransactionName(operation))
		ctx = span.Context()
		r.emitSignals(ctx, operation, outcome, code, reference, duration)
		span.Status = sentry.SpanStatusOK
		if outcome == "failed" || outcome == "rejected" {
			span.Status = sentry.SpanStatusInternalError
		}
		span.SetTag("component", r.component)
		span.SetTag("operation", operation)
		span.SetTag("outcome", outcome)
		span.SetTag("code", code)
		if reference != "" {
			span.SetTag("support_reference", reference)
		}
		span.Finish()
		return
	}
	r.emitSignals(ctx, operation, outcome, code, reference, duration)
}

func (r *Reporter) emitSignals(ctx context.Context, operation, outcome, code, reference string, duration time.Duration) {
	if r.logs && r.permit(&r.logsSent, &r.droppedLogs, 128) {
		entry := sentry.NewLogger(ctx).Info()
		if outcome == "failed" {
			entry = sentry.NewLogger(ctx).Error()
		}
		entry.String("component", r.component).String("operation", operation).String("outcome", outcome).String("code", code).String("support_reference", reference).Emit("paperboat.operation")
	}
	if r.metrics && r.permitN(&r.metricsSent, &r.droppedMetrics, maxDetailMetrics, 2) {
		meter := sentry.NewMeter(ctx)
		meter.SetAttributes(attribute.String("component", r.component), attribute.String("operation", operation), attribute.String("outcome", outcome), attribute.String("code", code))
		meter.Count("paperboat.operation.count", 1)
		if duration >= 0 {
			meter.Distribution("paperboat.operation.duration", duration.Seconds(), sentry.WithUnit(sentry.UnitSecond))
		}
	}
}

func (r *Reporter) permitN(counter *atomic.Uint32, dropped *atomic.Uint64, limit, count uint32) bool {
	r.refreshWindow()
	if counter.Add(count) <= limit {
		return true
	}
	dropped.Add(uint64(count))
	return false
}

func (r *Reporter) permit(counter *atomic.Uint32, dropped *atomic.Uint64, limit uint32) bool {
	r.refreshWindow()
	if counter.Add(1) > limit {
		dropped.Add(1)
		return false
	}
	return true
}

func (r *Reporter) refreshWindow() {
	minute := time.Now().Unix() / 60
	old := r.window.Load()
	if old != minute && r.window.CompareAndSwap(old, minute) {
		r.sent.Store(0)
		r.logsSent.Store(0)
		r.metricsSent.Store(0)
		r.tracesSent.Store(0)
	}
}

// ExportDrops publishes cumulative local rate-limit drops without recursive accounting.
func (r *Reporter) ExportDrops(ctx context.Context) {
	if r == nil || !r.enabled || r.closed.Load() || !r.metrics {
		return
	}
	ctx = sentry.SetHubOnContext(ctx, r.hub)
	meter := sentry.NewMeter(ctx)
	meter.SetAttributes(attribute.String("component", r.component))
	meter.Gauge("paperboat_reporting_logs_dropped_total_snapshot", float64(r.droppedLogs.Load()))
	meter.Gauge("paperboat_reporting_metrics_dropped_total_snapshot", float64(r.droppedMetrics.Load()))
	meter.Gauge("paperboat_reporting_errors_dropped_total_snapshot", float64(r.droppedErrors.Load()))
	meter.Gauge("paperboat_reporting_traces_dropped_total_snapshot", float64(r.droppedTraces.Load()))
	for signal, enabled := range map[string]bool{"errors": true, "logs": r.logs, "metrics": r.metrics, "traces": r.traces} {
		value := 0.0
		if enabled {
			value = 1
		}
		meter.SetAttributes(attribute.String("component", r.component), attribute.String("signal", signal))
		meter.Gauge("paperboat_reporting_signal_enabled", value)
	}
}

type Snapshot struct {
	Name, Kind string
	Value      float64
	Labels     map[string]string
}

// MetricSnapshots validates a complete cumulative snapshot and rotates through it so
// a large stable series set cannot starve its tail.
func (r *Reporter) MetricSnapshots(ctx context.Context, snapshots []Snapshot) {
	if r == nil || !r.enabled || r.closed.Load() || !r.metrics || len(snapshots) == 0 {
		return
	}
	valid := make([]Snapshot, 0, len(snapshots))
	for _, snapshot := range snapshots {
		if r.validSnapshot(snapshot) {
			valid = append(valid, snapshot)
		}
	}
	if len(valid) == 0 {
		return
	}
	count := len(valid)
	if count > maxSnapshots {
		count = maxSnapshots
	}
	start := int(r.snapshotCursor.Add(uint64(count))-uint64(count)) % len(valid)
	for i := 0; i < count; i++ {
		r.emitSnapshot(ctx, valid[(start+i)%len(valid)])
	}
}

func (r *Reporter) validSnapshot(snapshot Snapshot) bool {
	name, kind, value, labels := snapshot.Name, snapshot.Kind, snapshot.Value, snapshot.Labels
	outputName := name
	if kind == "counter" {
		outputName += "_snapshot"
	} else if kind == "histogram_sum" {
		outputName += "_sum_snapshot"
	} else if kind == "histogram_count" {
		outputName += "_count_snapshot"
	} else if kind != "gauge" {
		return false
	}
	spec, valid := metricCatalog[outputName]
	return !math.IsNaN(value) && !math.IsInf(value, 0) && valid && spec.kind == sentry.MetricTypeGauge && validLabels(spec.labels, labels)
}
func (r *Reporter) emitSnapshot(ctx context.Context, snapshot Snapshot) {
	name, kind, value, labels := snapshot.Name, snapshot.Kind, snapshot.Value, snapshot.Labels
	ctx = sentry.SetHubOnContext(ctx, r.hub)
	meter := sentry.NewMeter(ctx)
	attrs := []attribute.Builder{attribute.String("component", r.component)}
	for key, label := range labels {
		if safeToken(key) != key || safeToken(label) != label {
			return
		}
		attrs = append(attrs, attribute.String(key, label))
	}
	meter.SetAttributes(attrs...)
	switch kind {
	case "counter":
		meter.Gauge(name+"_snapshot", value)
	case "gauge":
		meter.Gauge(name, value)
	case "histogram_sum":
		meter.Gauge(name+"_sum_snapshot", value)
	case "histogram_count":
		meter.Gauge(name+"_count_snapshot", value)
	}
}
func validLabels(expected map[string]map[string]bool, labels map[string]string) bool {
	if len(expected) != len(labels) {
		return false
	}
	for key, value := range labels {
		allowed, ok := expected[key]
		if !ok || !allowed[value] {
			return false
		}
	}
	return true
}

// ControlTrace starts telemetry for a Paperboat-owned control request. Its support
// reference is always generated; sentry-trace is optional and baggage is never returned.
func (r *Reporter) ControlTrace(ctx context.Context, operation string) (string, string, func(string, string)) {
	reference := Reference()
	if r == nil || !r.enabled || r.closed.Load() || !operations[operation] {
		return "", reference, func(string, string) {}
	}
	started := time.Now()
	ctx = sentry.SetHubOnContext(ctx, r.hub)
	var span *sentry.Span
	if r.traces && r.permit(&r.tracesSent, &r.droppedTraces, maxTraces) {
		span = sentry.StartSpan(ctx, "paperboat."+operation, sentry.WithTransactionName(operation))
		ctx = span.Context()
		span.SetTag("component", r.component)
		span.SetTag("operation", operation)
		span.SetTag("support_reference", reference)
	}
	return func() string {
			if span != nil {
				return span.ToSentryTrace()
			}
			return ""
		}(), reference, func(outcome, code string) {
			if !outcomes[outcome] || !codes[code] || r.closed.Load() {
				return
			}
			if span != nil {
				span.SetTag("outcome", outcome)
				span.SetTag("code", code)
				if outcome == "failed" || outcome == "rejected" {
					span.Status = sentry.SpanStatusInternalError
				} else {
					span.Status = sentry.SpanStatusOK
				}
				span.Finish()
			}
			r.emitSignals(ctx, operation, outcome, code, reference, time.Since(started))
		}
}

func (r *Reporter) Capture(reference, kind string, skip int) {
	if r == nil || !r.enabled || r.closed.Load() || r.client == nil || !validReference(reference) || !r.permit(&r.sent, &r.droppedErrors, maxEvents) {
		return
	}
	event := &sentry.Event{
		Level: sentry.LevelError, Message: "unexpected service failure",
		Tags:      map[string]string{"failure_kind": failureKind(kind), "support_reference": reference},
		Exception: []sentry.Exception{{Type: "service_failure", Value: "unexpected service failure", Stacktrace: stack(skip + 1)}},
	}
	r.client.CaptureEvent(event, nil, nil)
}

func (r *Reporter) Close() {
	if r == nil || !r.closed.CompareAndSwap(false, true) || !r.enabled || r.client == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), flushTimeout)
	defer cancel()
	_ = r.client.FlushWithContext(ctx) // Queue drain is not an acknowledgement of server receipt.
	r.client.Close()
}

var entropy = rand.Reader

func Reference() string {
	var raw [16]byte
	if _, err := io.ReadFull(entropy, raw[:]); err != nil {
		return ""
	}
	return "pb-" + hex.EncodeToString(raw[:])
}

func validReference(value string) bool {
	if len(value) != 35 || !strings.HasPrefix(value, "pb-") {
		return false
	}
	_, err := hex.DecodeString(value[3:])
	return err == nil && strings.Trim(value, "0") != "" && value == strings.ToLower(value)
}

func stack(skip int) *sentry.Stacktrace {
	pcs := make([]uintptr, 32)
	n := runtime.Callers(skip+2, pcs)
	frames := runtime.CallersFrames(pcs[:n])
	result := &sentry.Stacktrace{}
	for len(result.Frames) < 24 {
		frame, more := frames.Next()
		result.Frames = append(result.Frames, safeFrame(sentry.Frame{Function: frame.Function, Filename: frame.File, Lineno: frame.Line}))
		if !more {
			break
		}
	}
	return result
}

func failureKind(value string) string {
	if value == "panic" || value == "service_run" {
		return value
	}
	return "internal"
}

func safeToken(value string) string {
	if value == "" {
		return ""
	}
	if len(value) > 128 {
		value = value[:128]
	}
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("._-()", r) {
			return r
		}
		return '_'
	}, value)
}

func safeFrame(frame sentry.Frame) sentry.Frame {
	name := strings.ReplaceAll(frame.Function, "\\", "/")
	if slash := strings.LastIndexByte(name, '/'); slash >= 0 {
		name = name[slash+1:]
	}
	module, function := safeToken(frame.Module), name
	if dot := strings.IndexByte(name, '.'); dot >= 0 {
		module, function = name[:dot], name[dot+1:]
	}
	filename := path.Base(strings.ReplaceAll(frame.Filename, "\\", "/"))
	return sentry.Frame{
		Filename: safeToken(filename), Module: safeToken(module), Function: safeToken(function),
		Lineno: frame.Lineno, InApp: true,
	}
}

func sanitize(event *sentry.Event, component, release string) *sentry.Event {
	return sanitizeEnvironment(event, component, release, "")
}
func sanitizeEnvironment(event *sentry.Event, component, release, environment string, fleet ...string) *sentry.Event {
	if event == nil {
		return nil
	}
	clean := &sentry.Event{
		EventID: event.EventID, Timestamp: event.Timestamp, Platform: "go",
		Level: sentry.LevelError, Message: "unexpected service failure", Release: release, Environment: environment,
		Tags: map[string]string{
			"component": safeToken(component), "failure_kind": failureKind(event.Tags["failure_kind"]),
			"support_reference": event.Tags["support_reference"],
		},
	}
	addFleetTags(clean.Tags, fleet)
	for _, exception := range event.Exception {
		if exception.Stacktrace == nil {
			continue
		}
		trace := &sentry.Stacktrace{Frames: make([]sentry.Frame, 0, len(exception.Stacktrace.Frames))}
		for _, frame := range exception.Stacktrace.Frames {
			trace.Frames = append(trace.Frames, safeFrame(frame))
		}
		clean.Exception = append(clean.Exception, sentry.Exception{Type: "service_failure", Value: "unexpected service failure", Stacktrace: trace})
	}
	return clean
}
