package reporting

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/getsentry/sentry-go"
	"github.com/getsentry/sentry-go/attribute"
)

type recordingTransport struct {
	mu         sync.Mutex
	events     []*sentry.Event
	flushed    bool
	closed     bool
	closeCalls int
}

func (*recordingTransport) Configure(sentry.ClientOptions) {}
func (t *recordingTransport) SendEvent(event *sentry.Event) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.events = append(t.events, event)
}
func (t *recordingTransport) Flush(time.Duration) bool { t.flushed = true; return true }
func (t *recordingTransport) FlushWithContext(context.Context) bool {
	t.flushed = true
	return true
}
func (t *recordingTransport) Close() { t.closed = true; t.closeCalls++ }

func TestConfigurationDefaultsAndOverrides(t *testing.T) {
	originalDSN, originalRelease := DefaultDSN, DefaultRelease
	t.Cleanup(func() { DefaultDSN, DefaultRelease = originalDSN, originalRelease })
	for _, name := range []string{"PAPERBOAT_SENTRY_ENABLED", "PAPERBOAT_SENTRY_DSN", "PAPERBOAT_SENTRY_RELEASE"} {
		t.Setenv(name, "")
	}
	const dsn = "https://public@example.invalid/1"
	tests := []struct {
		name, enabled, envDSN, envRelease, defaultDSN, defaultRelease string
		wantEnabled, wantError                                        bool
	}{
		{name: "source build off"},
		{name: "environment configuration needs opt in", envDSN: dsn, envRelease: "custom:1"},
		{name: "custom opt in", enabled: "true", envDSN: dsn, envRelease: "custom:1", wantEnabled: true},
		{name: "official default on", defaultDSN: dsn, defaultRelease: "paperboat-tunnel:1", wantEnabled: true},
		{name: "explicit disable", enabled: "false", defaultDSN: dsn, defaultRelease: "paperboat-tunnel:1"},
		{name: "environment overrides embedded", envDSN: "https://override@example.invalid/2", envRelease: "override:2", defaultDSN: dsn, defaultRelease: "paperboat-tunnel:1", wantEnabled: true},
		{name: "partial configuration invalid", defaultDSN: dsn, wantError: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("PAPERBOAT_SENTRY_ENABLED", test.enabled)
			t.Setenv("PAPERBOAT_SENTRY_DSN", test.envDSN)
			t.Setenv("PAPERBOAT_SENTRY_RELEASE", test.envRelease)
			DefaultDSN, DefaultRelease = test.defaultDSN, test.defaultRelease
			reporter, err := New("paperboat-tunnel")
			if (err != nil) != test.wantError {
				t.Fatalf("error=%v wantError=%t", err, test.wantError)
			}
			if err == nil && reporter.enabled != test.wantEnabled {
				t.Fatalf("enabled=%t want=%t", reporter.enabled, test.wantEnabled)
			}
			if err == nil {
				reporter.Close()
			}
		})
	}
}

func TestDisabledControlTraceStillCreatesCorrelationReference(t *testing.T) {
	reporter := &Reporter{}
	trace, reference, finish := reporter.ControlTrace(context.Background(), "dependency_health")
	if trace != "" || !validReference(reference) {
		t.Fatalf("trace=%q reference=%q", trace, reference)
	}
	finish("success", "ok")
}

func TestControlTraceRateLimitDropsAndRecoversNextWindow(t *testing.T) {
	client, err := sentry.NewClient(options("paperboat-tunnel", "release", "https://public@example.invalid/1", &recordingTransport{}))
	if err != nil {
		t.Fatal(err)
	}
	reporter := &Reporter{client: client, hub: sentry.NewHub(client, sentry.NewScope()), enabled: true, traces: true, component: "paperboat-tunnel"}
	for range maxTraces + 1 {
		_, reference, finish := reporter.ControlTrace(context.Background(), "dependency_health")
		if !validReference(reference) {
			t.Fatalf("reference=%q", reference)
		}
		finish("success", "ok")
	}
	if reporter.droppedTraces.Load() != 1 {
		t.Fatalf("drops=%d", reporter.droppedTraces.Load())
	}
	reporter.window.Store(reporter.window.Load() - 1)
	_, _, finish := reporter.ControlTrace(context.Background(), "dependency_health")
	finish("success", "ok")
	if reporter.tracesSent.Load() != 1 {
		t.Fatalf("window did not recover: %d", reporter.tracesSent.Load())
	}
}

func TestExplicitEnableRequiresConfiguration(t *testing.T) {
	originalDSN, originalRelease := DefaultDSN, DefaultRelease
	DefaultDSN, DefaultRelease = "", ""
	t.Cleanup(func() { DefaultDSN, DefaultRelease = originalDSN, originalRelease })
	t.Setenv("PAPERBOAT_SENTRY_ENABLED", "true")
	t.Setenv("PAPERBOAT_SENTRY_DSN", "")
	t.Setenv("PAPERBOAT_SENTRY_RELEASE", "")
	if _, err := New("paperboat-tunnel"); err == nil {
		t.Fatal("missing DSN and release accepted")
	}
}

func TestEnabledConfigurationAcceptsQualifiedRelease(t *testing.T) {
	t.Setenv("PAPERBOAT_SENTRY_ENABLED", "true")
	t.Setenv("PAPERBOAT_SENTRY_DSN", "https://public@example.invalid/1")
	t.Setenv("PAPERBOAT_SENTRY_RELEASE", "paperboat-tunnel:validation-20260919")
	t.Setenv("PAPERBOAT_SENTRY_REGION", "validation")
	t.Setenv("PAPERBOAT_SENTRY_INSTANCE", "slot-01")
	reporter, err := New("paperboat-tunnel")
	if err != nil || !reporter.enabled {
		t.Fatalf("qualified release rejected: %v", err)
	}
	reporter.Close()
}

func TestEnabledConfigurationRejectsUnsafeValues(t *testing.T) {
	for _, test := range []struct{ dsn, release string }{
		{"http://public@example.invalid/1", "2026.09.19.1"},
		{"https://public@example.invalid/1?secret=value", "2026.09.19.1"},
		{"https://public@example.invalid/1", "release/with/path"},
	} {
		t.Setenv("PAPERBOAT_SENTRY_ENABLED", "true")
		t.Setenv("PAPERBOAT_SENTRY_DSN", test.dsn)
		t.Setenv("PAPERBOAT_SENTRY_RELEASE", test.release)
		if _, err := New("paperboat-tunnel"); err == nil {
			t.Fatalf("accepted DSN %q release %q", test.dsn, test.release)
		}
	}
}

func TestFleetEnvironmentRequiresValidPair(t *testing.T) {
	for _, values := range [][2]string{{"in-blr", ""}, {"IN-BLR", "slot-01"}, {"in-blr", "slot-65"}} {
		t.Setenv("PAPERBOAT_SENTRY_REGION", values[0])
		t.Setenv("PAPERBOAT_SENTRY_INSTANCE", values[1])
		if _, _, err := fleetEnvironment(); err == nil {
			t.Fatalf("accepted fleet values %q", values)
		}
	}
	t.Setenv("PAPERBOAT_SENTRY_REGION", "in-blr")
	t.Setenv("PAPERBOAT_SENTRY_INSTANCE", "slot-64")
	if region, instance, err := fleetEnvironment(); err != nil || region != "in-blr" || instance != "slot-64" {
		t.Fatalf("fleet=%q/%q err=%v", region, instance, err)
	}
}

func TestSanitizesAndBoundsEvents(t *testing.T) {
	transport := &recordingTransport{}
	client, err := sentry.NewClient(options("paperboat-tunnel", "2026.09.19.1", "https://public@example.invalid/1", transport))
	if err != nil {
		t.Fatal(err)
	}
	reporter := &Reporter{client: client, enabled: true}
	ref := "pb-0123456789abcdef0123456789abcdef"
	for i := 0; i < maxEvents+3; i++ {
		reporter.Capture(ref, "service/run secret=value /private/path", 0)
	}
	reporter.Close()
	if len(transport.events) != maxEvents {
		t.Fatalf("events=%d", len(transport.events))
	}
	if !transport.flushed || !transport.closed {
		t.Fatalf("flushed=%t closed=%t", transport.flushed, transport.closed)
	}
	event := transport.events[0]
	if event.Request != nil || len(event.Breadcrumbs) != 0 || len(event.Contexts) != 0 || len(event.Modules) != 0 || event.User.ID != "" {
		t.Fatalf("unsafe SDK fields retained: %+v", event)
	}
	if event.Tags["support_reference"] != ref || event.Tags["component"] != "paperboat-tunnel" {
		t.Fatalf("tags=%v", event.Tags)
	}
	if event.Message != "unexpected service failure" || event.Release != "2026.09.19.1" {
		t.Fatalf("message=%q release=%q", event.Message, event.Release)
	}
	for _, exception := range event.Exception {
		if strings.Contains(exception.Value, "secret") || exception.Stacktrace == nil {
			t.Fatalf("exception=%+v", exception)
		}
		for _, frame := range exception.Stacktrace.Frames {
			if frame.Filename == "" || frame.Module == "" || frame.Function == "" || frame.AbsPath != "" || frame.Vars != nil || strings.Contains(frame.Function, "/") {
				t.Fatalf("unsafe frame=%+v", frame)
			}
		}
	}
}

func TestSanitizeReconstructsAllowlistedEvent(t *testing.T) {
	ref := "pb-0123456789abcdef0123456789abcdef"
	dirty := &sentry.Event{
		Message: "token=secret", Logger: "private", Transaction: "/users/alice", ServerName: "host",
		Environment: "private", Request: &sentry.Request{URL: "https://secret.invalid/path"},
		Contexts: map[string]sentry.Context{"secret": {"value": "private"}},
		Tags:     map[string]string{"support_reference": ref, "failure_kind": "service/run", "secret": "private"},
		Exception: []sentry.Exception{{Value: "password", Stacktrace: &sentry.Stacktrace{Frames: []sentry.Frame{{
			Function: "github.com/private/project/internal/service.run", Filename: "/home/alice/private/service.go",
			AbsPath: "/home/alice/private/service.go", Vars: map[string]any{"token": "secret"}, Lineno: 42,
		}}}}},
	}
	clean := sanitize(dirty, "paperboat-tunnel", "2026.09.19.1")
	if clean.Request != nil || clean.Contexts != nil || clean.Logger != "" || clean.Transaction != "" || clean.ServerName != "" || clean.Environment != "" {
		t.Fatalf("unsafe fields survived: %+v", clean)
	}
	frame := clean.Exception[0].Stacktrace.Frames[0]
	if frame.Filename != "service.go" || frame.Module != "service" || frame.Function != "run" || frame.Lineno != 42 || frame.AbsPath != "" || frame.Vars != nil {
		t.Fatalf("frame=%+v", frame)
	}
}

func TestReferenceFormat(t *testing.T) {
	if reference := Reference(); !validReference(reference) {
		t.Fatalf("invalid reference %q", reference)
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("entropy unavailable") }

func TestReferenceEntropyFailureHasNoSharedFallback(t *testing.T) {
	old := entropy
	entropy = failingReader{}
	t.Cleanup(func() { entropy = old })
	if reference := Reference(); reference != "" {
		t.Fatalf("reference=%q", reference)
	}
}

func TestSignalConfigurationIsIndependentAndBounded(t *testing.T) {
	t.Setenv("PAPERBOAT_SENTRY_ENABLED", "true")
	t.Setenv("PAPERBOAT_SENTRY_DSN", "https://public@example.invalid/1")
	t.Setenv("PAPERBOAT_SENTRY_RELEASE", "2026.09.19.1")
	t.Setenv("PAPERBOAT_SENTRY_LOGS_ENABLED", "false")
	t.Setenv("PAPERBOAT_SENTRY_METRICS_ENABLED", "true")
	t.Setenv("PAPERBOAT_SENTRY_TRACES_SAMPLE_RATE", "0")
	reporter, err := New("paperboat-tunnel")
	if err != nil {
		t.Fatal(err)
	}
	defer reporter.Close()
	if reporter.logs || !reporter.metrics || reporter.traces {
		t.Fatalf("signals logs=%t metrics=%t traces=%t", reporter.logs, reporter.metrics, reporter.traces)
	}
	reporter.Observe(context.Background(), "raw/path", "failed", "internal", "secret", time.Second)
	if reporter.logsSent.Load() != 0 || reporter.metricsSent.Load() != 0 {
		t.Fatal("unsafe observation was emitted")
	}
}

func TestRejectsInvalidSignalConfiguration(t *testing.T) {
	t.Setenv("PAPERBOAT_SENTRY_ENABLED", "true")
	t.Setenv("PAPERBOAT_SENTRY_DSN", "https://public@example.invalid/1")
	t.Setenv("PAPERBOAT_SENTRY_RELEASE", "2026.09.19.1")
	t.Setenv("PAPERBOAT_SENTRY_TRACES_SAMPLE_RATE", "1.1")
	if _, err := New("paperboat-tunnel"); err == nil {
		t.Fatal("invalid trace sample rate accepted")
	}
}

func TestOperationEmitsLogMetricAndTraceEnvelopes(t *testing.T) {
	transport := &recordingTransport{}
	config := options("paperboat-tunnel", "2026.09.19.1", "https://public@example.invalid/1", transport)
	config.EnableTracing = true
	config.TracesSampleRate = 1
	config.BeforeSend = func(event *sentry.Event, _ *sentry.EventHint) *sentry.Event {
		return sanitizeEnvironment(event, "paperboat-tunnel", "2026.09.19.1", "production", "in-blr", "slot-01")
	}
	config.BeforeSendLog = func(log *sentry.Log) *sentry.Log {
		return sanitizeLog(log, "paperboat-tunnel", "2026.09.19.1", "production", "in-blr", "slot-01")
	}
	config.BeforeSendMetric = func(metric *sentry.Metric) *sentry.Metric {
		return sanitizeMetric(metric, "paperboat-tunnel", "2026.09.19.1", "production", "in-blr", "slot-01")
	}
	config.BeforeSendTransaction = func(event *sentry.Event, _ *sentry.EventHint) *sentry.Event {
		return sanitizeTransaction(event, "paperboat-tunnel", "2026.09.19.1", "production", "in-blr", "slot-01")
	}
	client, err := sentry.NewClient(config)
	if err != nil {
		t.Fatal(err)
	}
	reporter := &Reporter{client: client, hub: sentry.NewHub(client, sentry.NewScope()), enabled: true, logs: true, traces: true, metrics: true, component: "paperboat-tunnel"}
	const reference = "pb-0123456789abcdef0123456789abcdef"
	reporter.Observe(context.Background(), "connector_attach", "failed", "unavailable", reference, time.Second)
	reporter.Capture(reference, "service_run", 0)
	reporter.ExportDrops(context.Background())
	reporter.Close()
	var logs, metrics, transactions int
	for _, event := range transport.events {
		if len(event.Exception) > 0 && (event.Tags["region"] != "in-blr" || event.Tags["instance"] != "slot-01") {
			t.Fatalf("error fleet tags=%v", event.Tags)
		}
		logs += len(event.Logs)
		metrics += len(event.Metrics)
		for _, metric := range event.Metrics {
			if metric.Attributes["region"].AsString() != "in-blr" || metric.Attributes["instance"].AsString() != "slot-01" {
				t.Fatalf("metric fleet=%v", metric.Attributes)
			}
			if metric.TraceID != (sentry.TraceID{}) || metric.SpanID != (sentry.SpanID{}) {
				t.Fatalf("metric retained trace identity: %+v", metric)
			}
		}
		if event.Type == "transaction" {
			transactions++
			if event.Tags["region"] != "in-blr" || event.Tags["instance"] != "slot-01" {
				t.Fatalf("trace fleet tags=%v", event.Tags)
			}
			trace := event.Contexts["trace"]
			traceID, traceOK := trace["trace_id"].(sentry.TraceID)
			spanID, spanOK := trace["span_id"].(sentry.SpanID)
			raw, err := json.Marshal(event)
			if err != nil || !traceOK || !spanOK || traceID == (sentry.TraceID{}) || spanID == (sentry.SpanID{}) || !strings.Contains(string(raw), traceID.String()) || !strings.Contains(string(raw), spanID.String()) || !strings.Contains(string(raw), reference) {
				t.Fatalf("serialized transaction lost correlation: %s err=%v", raw, err)
			}
		}
		for _, log := range event.Logs {
			if log.Attributes["region"].AsString() != "in-blr" || log.Attributes["instance"].AsString() != "slot-01" {
				t.Fatalf("log fleet=%v", log.Attributes)
			}
			if log.Body != "paperboat.operation" || log.Attributes["operation"].AsInterface() != "connector_attach" {
				t.Fatalf("unsafe log=%+v", log)
			}
		}
	}
	if logs != 1 || metrics != 10 || transactions != 1 {
		t.Fatalf("envelopes logs=%d metrics=%d transactions=%d", logs, metrics, transactions)
	}
}

func TestOperationRateLimitDropsAndRecoversNextWindow(t *testing.T) {
	transport := &recordingTransport{}
	client, err := sentry.NewClient(options("paperboat-tunnel", "2026.09.19.1", "https://public@example.invalid/1", transport))
	if err != nil {
		t.Fatal(err)
	}
	reporter := &Reporter{client: client, hub: sentry.NewHub(client, sentry.NewScope()), enabled: true, logs: true, component: "paperboat-tunnel"}
	for range 129 {
		reporter.Observe(context.Background(), "connector_stream", "success", "ok", "", 0)
	}
	if reporter.droppedLogs.Load() != 1 {
		t.Fatalf("drops=%d", reporter.droppedLogs.Load())
	}
	reporter.window.Store(reporter.window.Load() - 1)
	reporter.Observe(context.Background(), "connector_stream", "success", "recovered", "", 0)
	if reporter.logsSent.Load() != 1 {
		t.Fatalf("window did not recover: %d", reporter.logsSent.Load())
	}
	reporter.Close()
}

func TestSnapshotRotationSurvivesSaturatedDetailMetrics(t *testing.T) {
	transport := &recordingTransport{}
	client, err := sentry.NewClient(options("paperboat-tunnel", "release", "https://public@example.invalid/1", transport))
	if err != nil {
		t.Fatal(err)
	}
	reporter := &Reporter{client: client, hub: sentry.NewHub(client, sentry.NewScope()), enabled: true, metrics: true, component: "paperboat-tunnel"}
	for range maxDetailMetrics/2 + 1 {
		reporter.Observe(context.Background(), "connector_stream", "success", "ok", "", 0)
	}
	snapshots := make([]Snapshot, maxSnapshots+1)
	for i := range maxSnapshots {
		snapshots[i] = Snapshot{Name: "paperboat_edge_config_desired_generation", Kind: "gauge", Value: float64(i)}
	}
	snapshots[maxSnapshots] = Snapshot{Name: "paperboat_edge_config_applied_generation", Kind: "gauge", Value: 65}
	reporter.MetricSnapshots(context.Background(), snapshots)
	reporter.MetricSnapshots(context.Background(), snapshots)
	reporter.Close()
	if reporter.snapshotCursor.Load() != 2*maxSnapshots {
		t.Fatalf("cursor=%d", reporter.snapshotCursor.Load())
	}
	found := false
	for _, event := range transport.events {
		for _, metric := range event.Metrics {
			if metric.Name == "paperboat_edge_config_applied_generation" {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("rotated tail snapshot was not exported")
	}
}

func TestSanitizersRejectMaliciousTraceAndMetricFields(t *testing.T) {
	ref := "pb-0123456789abcdef0123456789abcdef"
	event := &sentry.Event{Transaction: "connector_stream", Tags: map[string]string{"outcome": "failed", "code": "internal", "support_reference": ref, "secret": "credential"}, Contexts: map[string]sentry.Context{"trace": {"trace_id": "0123456789abcdef0123456789abcdef", "span_id": "0123456789abcdef", "parent_span_id": "fedcba9876543210", "op": "secret/path", "description": "token=secret", "data": map[string]any{"key": "secret"}}, "request": {"url": "https://secret.invalid"}}}
	clean := sanitizeTransaction(event, "paperboat-tunnel", "release", "production")
	if clean == nil {
		t.Fatal("valid trace rejected")
	}
	trace := clean.Contexts["trace"]
	if len(trace) != 2 || trace["op"] != "paperboat.connector_stream" || trace["description"] != nil || clean.Contexts["request"] != nil || clean.Tags["secret"] != "" || clean.Tags["support_reference"] != ref {
		t.Fatalf("unsafe transaction=%+v", clean)
	}
	malicious := &sentry.Metric{Name: "paperboat_edge_SECRET", Type: sentry.MetricTypeGauge, Attributes: map[string]attribute.Value{"secret": attribute.StringValue("credential")}}
	if sanitizeMetric(malicious, "paperboat-tunnel", "release", "production") != nil {
		t.Fatal("unknown prefixed metric accepted")
	}
	wrongEnum := &sentry.Metric{Name: "paperboat_edge_queue_depth", Type: sentry.MetricTypeGauge, Attributes: map[string]attribute.Value{"queue": attribute.StringValue("account_123")}}
	if sanitizeMetric(wrongEnum, "paperboat-tunnel", "release", "production") != nil {
		t.Fatal("unknown label enum accepted")
	}
}

func TestErrorRateLimitRecoversAndCloseIsIdempotent(t *testing.T) {
	transport := &recordingTransport{}
	client, err := sentry.NewClient(options("paperboat-tunnel", "release", "https://public@example.invalid/1", transport))
	if err != nil {
		t.Fatal(err)
	}
	reporter := &Reporter{client: client, hub: sentry.NewHub(client, sentry.NewScope()), enabled: true, component: "paperboat-tunnel"}
	ref := "pb-0123456789abcdef0123456789abcdef"
	for range maxEvents + 1 {
		reporter.Capture(ref, "service_run", 0)
	}
	if reporter.droppedErrors.Load() != 1 {
		t.Fatalf("drops=%d", reporter.droppedErrors.Load())
	}
	reporter.window.Store(reporter.window.Load() - 1)
	reporter.Capture(ref, "service_run", 0)
	if reporter.sent.Load() != 1 {
		t.Fatalf("window did not recover: %d", reporter.sent.Load())
	}
	reporter.Close()
	reporter.Close()
	if transport.closeCalls != 1 {
		t.Fatalf("close calls=%d", transport.closeCalls)
	}
}
