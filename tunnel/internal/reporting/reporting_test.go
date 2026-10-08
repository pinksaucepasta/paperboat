package reporting

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/getsentry/sentry-go"
	"github.com/getsentry/sentry-go/attribute"
	"github.com/google/uuid"
	"github.com/pinksaucepasta/paperboat-tunnel/internal/control"
	"github.com/pinksaucepasta/paperboat-tunnel/internal/route"
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

func waitForEnvelopeContent(t *testing.T, mu *sync.Mutex, envelopes *[][]byte, notifications <-chan struct{}, expected ...string) []byte {
	t.Helper()
	timer := time.NewTimer(flushTimeout)
	defer timer.Stop()
	for {
		mu.Lock()
		payload := bytes.Join(*envelopes, nil)
		mu.Unlock()
		complete := true
		for _, name := range expected {
			if !bytes.Contains(payload, []byte(name)) {
				complete = false
				break
			}
		}
		if complete {
			return payload
		}
		select {
		case <-notifications:
		case <-timer.C:
			t.Fatalf("timed out waiting for telemetry envelope content %v", expected)
			return payload
		}
	}
}

type privateFailure struct{ errorCalls *atomic.Int32 }

func (e *privateFailure) Error() string {
	e.errorCalls.Add(1)
	return "token=must-not-be-read /private/customer/path"
}

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
	reporter := &Reporter{client: client, enabled: true, component: "paperboat-tunnel", localFault: func(Fault) {}}
	ref := "support_5b8a43c1-574f-4c56-94e1-a1adf6fc2c83"
	for i := 0; i < maxEvents+3; i++ {
		reporter.CaptureFailure(WithSupportReference(context.Background(), ref), "service_run", errors.New("token=secret /private/path"))
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
	if event.Message != "paperboat service failure" || event.Release != "2026.09.19.1" {
		t.Fatalf("message=%q release=%q", event.Message, event.Release)
	}
	if event.Tags["schema"] != "" || event.Tags["operation"] != "service_lifecycle" || event.Tags["stage"] != "serve" || event.Tags["code"] != "service_failed" || event.Tags["cause"] != "internal" || event.Tags["outcome"] != "failed" || event.Tags["error_type"] != "errorString" {
		t.Fatalf("fault tags=%v", event.Tags)
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
	ref := "support_5b8a43c1-574f-4c56-94e1-a1adf6fc2c83"
	dirty := &sentry.Event{
		Message: "token=secret", Logger: "private", Transaction: "/users/alice", ServerName: "host",
		Environment: "private", Request: &sentry.Request{URL: "https://secret.invalid/path"},
		Contexts: map[string]sentry.Context{"secret": {"value": "private"}},
		Tags:     map[string]string{"support_reference": ref, "correlation_id": ref, "operation": "service_lifecycle", "stage": "serve", "name": "private", "code": "service_failed", "cause": "internal", "error_type": "privateFailure", "outcome": "failed", "secret": "private"},
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
	if clean.Tags["name"] != "tunnel_service_failed" || clean.Tags["error_type"] != "privateFailure" || clean.Tags["secret"] != "" || clean.Exception[0].Value != "internal" {
		t.Fatalf("fault sanitizer did not reconstruct safe event: %+v", clean)
	}
}

func TestCaptureFailureOfflineProjectsSafeCauseAndPreservesReference(t *testing.T) {
	var local Fault
	calls := atomic.Int32{}
	const reference = "support_5b8a43c1-574f-4c56-94e1-a1adf6fc2c83"
	reporter := &Reporter{component: "paperboat-tunnel", localFault: func(fault Fault) { local = fault }}
	err := errors.Join(control.ErrControlUnavailable, &privateFailure{errorCalls: &calls})
	fault := reporter.CaptureFailure(WithSupportReference(context.Background(), reference), "service_run", err)
	if calls.Load() != 0 {
		t.Fatalf("original error string was evaluated %d times", calls.Load())
	}
	if fault.Schema != "paperboat.edge_event.v1" || fault.Component != "paperboat-tunnel" || fault.Operation != "service_lifecycle" || fault.Stage != "serve" || fault.Name != "tunnel_service_failed" || fault.Code != "service_failed" || fault.Cause != "control_unavailable" || fault.Outcome != "failed" || fault.CorrelationID != reference {
		t.Fatalf("fault=%+v", fault)
	}
	if local.CorrelationID != reference || local.SupportReference != reference || local.Cause != fault.Cause || local.ErrorType != fault.ErrorType || len(local.ErrorChain) == 0 || local.SourceFile == "" || local.SourceFunction == "" || local.SourceLine <= 0 {
		t.Fatalf("local fault=%+v; projected fault=%+v", local, fault)
	}
}

func TestConfigurationFailureIsLocalRejection(t *testing.T) {
	var local Fault
	reporter := &Reporter{component: "paperboat-tunnel", localFault: func(fault Fault) { local = fault }}
	fault := reporter.CaptureFailure(context.Background(), "service_config", errors.New("private config path and body"))
	if fault.Stage != "configure" || fault.Code != "service_setup_failed" || fault.Outcome != "rejected" || fault.Severity != "warning" || fault.CorrelationID == "" || local.CorrelationID != fault.CorrelationID {
		t.Fatalf("configuration fault=%+v local=%+v", fault, local)
	}
}

func TestCaptureFailureReusesControlRequestReference(t *testing.T) {
	var local Fault
	const reference = "support_fedcba98-7654-4a32-8fed-cba987654321"
	reporter := &Reporter{component: "paperboat-tunnel", localFault: func(fault Fault) { local = fault }}
	err := &control.RequestFailure{Path: "/v1/edge/routes/desired-state", Status: http.StatusServiceUnavailable, Category: "transport", SupportReference: reference, Err: control.ErrControlUnavailable}
	fault := reporter.CaptureFailure(context.Background(), "route_reconcile", err)
	if fault.CorrelationID != reference || fault.SupportReference != reference || local.CorrelationID != reference || local.SupportReference != reference || fault.Cause != "service_unavailable" || fault.HTTPStatus != http.StatusServiceUnavailable || fault.Stage != "reconcile" || fault.Code != "control_reconcile_failed" {
		t.Fatalf("fault=%+v local=%+v", fault, local)
	}
	rejected := reporter.CaptureFailure(WithSupportReference(context.Background(), reference), "route_reconcile", &control.RequestFailure{Status: http.StatusForbidden, SupportReference: reference})
	if rejected.Cause != "permission_denied" || rejected.HTTPStatus != http.StatusForbidden || rejected.Outcome != "rejected" || rejected.Severity != "warning" || local.Outcome != "rejected" || local.Severity != "warning" {
		t.Fatalf("rejected fault=%+v local=%+v", rejected, local)
	}
}

func TestFaultProjectionIncludesSafeErrno(t *testing.T) {
	var local Fault
	reporter := &Reporter{localFault: func(fault Fault) { local = fault }}
	const reference = "support_5b8a43c1-574f-4c56-94e1-a1adf6fc2c83"
	fault := reporter.CaptureFailure(WithSupportReference(context.Background(), reference), "service_run", syscall.ECONNREFUSED)
	if fault.Cause != "connection_refused" || fault.Errno != int(syscall.ECONNREFUSED) || len(fault.ErrorChain) == 0 || fault.CorrelationID != reference || fault.SupportReference != reference || local.Errno != fault.Errno {
		t.Fatalf("fault=%+v local=%+v", fault, local)
	}
}

func TestCaptureFailureCancellationIsInformationalWithoutException(t *testing.T) {
	transport := &recordingTransport{}
	client, err := sentry.NewClient(options("paperboat-tunnel", "release", "https://public@example.invalid/1", transport))
	if err != nil {
		t.Fatal(err)
	}
	var local Fault
	reporter := &Reporter{client: client, hub: sentry.NewHub(client, sentry.NewScope()), enabled: true, component: "paperboat-tunnel", localFault: func(fault Fault) { local = fault }}
	const reference = "support_5b8a43c1-574f-4c56-94e1-a1adf6fc2c83"
	fault := reporter.CaptureFailure(WithSupportReference(context.Background(), reference), "service_run", context.Canceled)
	reporter.Close()
	if fault.Outcome != "canceled" || fault.Severity != "info" || fault.Cause != "context_canceled" || local.CorrelationID != reference {
		t.Fatalf("fault=%+v local=%+v", fault, local)
	}
	for _, event := range transport.events {
		if len(event.Exception) != 0 {
			t.Fatalf("cancellation produced exception: %+v", event.Exception)
		}
	}
}

func TestReferenceFormat(t *testing.T) {
	seen := map[string]bool{}
	for range 2 {
		reference := Reference()
		id, err := uuid.Parse(strings.TrimPrefix(reference, "support_"))
		if err != nil || id.Version() != 4 || id.Variant() != uuid.RFC4122 || "support_"+id.String() != reference || !validReference(reference) {
			t.Fatalf("invalid reference %q", reference)
		}
		if seen[reference] {
			t.Fatal("support reference reused")
		}
		seen[reference] = true
	}
}

func TestSupportReferenceBounds(t *testing.T) {
	for _, reference := range []string{"support_5b8a43c1-574f-4c56-94e1-a1adf6fc2c83"} {
		if !validReference(reference) {
			t.Errorf("valid reference rejected: %q", reference)
		}
	}
	for _, reference := range []string{"", "support_5b8a43c1-574f-1c56-94e1-a1adf6fc2c83", "support_5b8a43c1-574f-4c56-74e1-a1adf6fc2c83", "support_5B8A43C1-574F-4C56-94E1-A1ADF6FC2C83", "support_5b8a43c1-574f-4c56-94e1-a1adf6fc2c83\n", strings.Repeat("a", 129)} {
		if validReference(reference) {
			t.Errorf("invalid reference accepted: %q", reference)
		}
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
	const reference = "support_5b8a43c1-574f-4c56-94e1-a1adf6fc2c83"
	reporter.Observe(context.Background(), "connector_attach", "failed", "unavailable", reference, time.Second)
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

func TestCanceledSpanUsesCanceledSDKStatus(t *testing.T) {
	transport := &recordingTransport{}
	config := options("paperboat-tunnel", "release", "https://public@example.invalid/1", transport)
	config.EnableTracing = true
	config.TracesSampleRate = 1
	config.BeforeSendTransaction = func(event *sentry.Event, _ *sentry.EventHint) *sentry.Event {
		return sanitizeTransaction(event, "paperboat-tunnel", "release", "production")
	}
	client, err := sentry.NewClient(config)
	if err != nil {
		t.Fatal(err)
	}
	reporter := &Reporter{client: client, hub: sentry.NewHub(client, sentry.NewScope()), enabled: true, traces: true, component: "paperboat-tunnel"}
	reporter.Observe(context.Background(), "service_lifecycle", "canceled", "shutdown", "", time.Millisecond)
	reporter.Close()
	for _, event := range transport.events {
		if event.Type == "transaction" {
			if event.Contexts["trace"]["status"] != "cancelled" {
				t.Fatalf("canceled transaction status=%v", event.Contexts["trace"]["status"])
			}
			return
		}
	}
	t.Fatal("canceled transaction was not captured")
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
	var mu sync.Mutex
	var envelopes [][]byte
	notifications := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		payload, err := io.ReadAll(request.Body)
		if err != nil {
			t.Errorf("read Sentry envelope: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		mu.Lock()
		envelopes = append(envelopes, bytes.Clone(payload))
		mu.Unlock()
		select {
		case notifications <- struct{}{}:
		default:
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	dsn := "http://public@" + strings.TrimPrefix(server.URL, "http://") + "/1"
	transport := sentry.NewHTTPTransport()
	transport.BufferSize, transport.Timeout = maxEvents, flushTimeout
	clientOptions := options("paperboat-tunnel", "release", dsn, transport)
	client, err := sentry.NewClient(clientOptions)
	if err != nil {
		t.Fatal(err)
	}
	reporter := &Reporter{client: client, hub: sentry.NewHub(client, sentry.NewScope()), enabled: true, metrics: true, component: "paperboat-tunnel"}
	for range maxDetailMetrics/2 + 1 {
		reporter.Observe(context.Background(), "connector_stream", "success", "ok", "", 0)
	}
	snapshots := make([]Snapshot, maxSnapshots+1)
	snapshots[0] = Snapshot{Name: "paperboat_edge_config_desired_generation", Kind: "gauge", Value: 64}
	snapshots[1] = Snapshot{Name: "paperboat_edge_config_applied_generation", Kind: "gauge", Value: 64}
	snapshots[2] = Snapshot{Name: "paperboat_edge_service_uptime_seconds", Kind: "gauge", Value: 86400}
	for i := 3; i < maxSnapshots; i++ {
		snapshots[i] = Snapshot{Name: "paperboat_edge_config_desired_generation", Kind: "gauge", Value: float64(i)}
	}
	snapshots[maxSnapshots] = Snapshot{Name: "paperboat_edge_crash_loop", Kind: "gauge", Value: 0}
	// The exact production SDK path has a 100-item queue. This window
	// contains 28 detailed items, eight health gauges and 64 snapshots.
	reporter.ExportDrops(context.Background())
	reporter.MetricSnapshots(context.Background(), snapshots)
	// Flush only after the complete window; no flush separates detail and health.
	if !client.Flush(flushTimeout) {
		t.Fatal("first snapshot window did not flush")
	}
	mu.Lock()
	firstWindow := bytes.Join(envelopes, nil)
	mu.Unlock()
	for _, name := range []string{"paperboat_edge_config_desired_generation", "paperboat_edge_config_applied_generation", "paperboat_edge_service_uptime_seconds", "paperboat_reporting_metrics_dropped_total_snapshot"} {
		if !bytes.Contains(firstWindow, []byte(name)) {
			t.Fatalf("health snapshot missing after production drain: %s", name)
		}
	}
	if count := metricItemCount(t, firstWindow); count != 100 {
		t.Fatalf("first-window metric items=%d, want 100", count)
	}
	reporter.ExportDrops(context.Background())
	reporter.MetricSnapshots(context.Background(), snapshots)
	reporter.Close()
	if reporter.snapshotCursor.Load() != 2*maxSnapshots {
		t.Fatalf("cursor=%d", reporter.snapshotCursor.Load())
	}
	waitForEnvelopeContent(t, &mu, &envelopes, notifications, "paperboat_edge_crash_loop", "paperboat_edge_config_desired_generation", "paperboat_reporting_metrics_dropped_total_snapshot")
}

func TestSanitizersRejectMaliciousTraceAndMetricFields(t *testing.T) {
	ref := "support_5b8a43c1-574f-4c56-94e1-a1adf6fc2c83"
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
	reporter := &Reporter{client: client, hub: sentry.NewHub(client, sentry.NewScope()), enabled: true, component: "paperboat-tunnel", localFault: func(Fault) {}}
	ref := "support_5b8a43c1-574f-4c56-94e1-a1adf6fc2c83"
	for range maxEvents + 1 {
		reporter.CaptureFailure(WithSupportReference(context.Background(), ref), "service_run", errors.New("private failure"))
	}
	if reporter.droppedErrors.Load() != 1 {
		t.Fatalf("drops=%d", reporter.droppedErrors.Load())
	}
	reporter.window.Store(reporter.window.Load() - 1)
	reporter.CaptureFailure(WithSupportReference(context.Background(), ref), "service_run", errors.New("private failure"))
	if reporter.sent.Load() != 1 {
		t.Fatalf("window did not recover: %d", reporter.sent.Load())
	}
	reporter.Close()
	reporter.Close()
	if transport.closeCalls != 1 {
		t.Fatalf("close calls=%d", transport.closeCalls)
	}
}

func TestConcurrentErrorQuotaNeverExceedsWindowLimit(t *testing.T) {
	reporter := &Reporter{}
	const attempts = 256
	start := make(chan struct{})
	var wg sync.WaitGroup
	var mu sync.Mutex
	accepted := 0
	for range attempts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if reporter.permit(&reporter.sent, &reporter.droppedErrors, maxEvents) {
				mu.Lock()
				accepted++
				mu.Unlock()
			}
		}()
	}
	close(start)
	wg.Wait()
	if accepted != maxEvents || reporter.sent.Load() != maxEvents || reporter.droppedErrors.Load() != attempts-maxEvents {
		t.Fatalf("accepted=%d sent=%d dropped=%d", accepted, reporter.sent.Load(), reporter.droppedErrors.Load())
	}
}

func TestObservedControlFailureIsLocalWithoutDuplicateCapture(t *testing.T) {
	var local []Fault
	disabled := &Reporter{localFault: func(f Fault) { local = append(local, f) }}
	const reference = "support_01234567-89ab-4def-8123-456789abcdef"
	ctx := WithSupportReference(context.Background(), reference)
	fault := disabled.ObserveFailure(ctx, "control_request", &statusFailure{status: 503})
	if len(local) != 1 || fault.Cause != "service_unavailable" || fault.HTTPStatus != 503 || fault.CorrelationID != reference || fault.SourceFile != "reporting_test.go" {
		t.Fatalf("offline fault=%+v", fault)
	}
	transport := &recordingTransport{}
	config := options("paperboat-tunnel", "release", "https://public@example.invalid/1", transport)
	client, err := sentry.NewClient(config)
	if err != nil {
		t.Fatal(err)
	}
	reporter := &Reporter{client: client, hub: sentry.NewHub(client, sentry.NewScope()), enabled: true, logs: true, metrics: true, component: "paperboat-tunnel", localFault: func(f Fault) { local = append(local, f) }}
	_, ref, finish := reporter.ControlTrace(context.Background(), "dependency_health")
	reporter.ObserveFailure(WithSupportReference(context.Background(), ref), "control_request", &statusFailure{status: 503})
	finish("failed", "control_request_failed")
	reporter.Close()
	logs, metrics, exceptions := 0, 0, 0
	for _, event := range transport.events {
		logs += len(event.Logs)
		metrics += len(event.Metrics)
		exceptions += len(event.Exception)
	}
	if len(local) != 2 || logs != 1 || metrics != 2 || exceptions != 0 {
		t.Fatalf("local=%d logs=%d metricitems=%d exceptions=%d", len(local), logs, metrics, exceptions)
	}
}

// statusFailure deliberately contains private text to exercise the safe projection.
type statusFailure struct{ status int }

func (*statusFailure) Error() string           { return "token=private /customer/path" }
func (e *statusFailure) DiagnosticStatus() int { return e.status }

func TestRouteLifecycleProductionHTTPExportPreservesFiniteProjection(t *testing.T) {
	var mu sync.Mutex
	var payload []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, err := io.ReadAll(req.Body)
		if err != nil {
			t.Error(err)
		}
		mu.Lock()
		payload = append(payload, body...)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	reporter := productionFixture(t, server.URL, productionHTTPTransport(), true, false, false)
	defer reporter.Close()
	const reference = "support_5b8a43c1-574f-4c56-94e1-a1adf6fc2c83"
	kinds := []route.LifecycleType{route.LifecycleStageAccepted, route.LifecycleStageRejected, route.LifecycleGenerationReady, route.LifecycleActivated, route.LifecycleDrainStarted, route.LifecycleDrainCompleted, route.LifecycleDrainForced, route.LifecycleStaleRejected, route.LifecycleStreamAcquired, route.LifecycleStreamOverloaded, route.LifecycleStreamReleased}
	for _, kind := range kinds {
		input, err := (route.RouteTelemetryRecord{Type: kind}).EventInput()
		if err != nil {
			t.Fatal(err)
		}
		reporter.Observe(t.Context(), "route_lifecycle", string(input.Outcome), input.Code, reference, -1)
		attrs := map[string]attribute.Value{"operation": attribute.StringValue("route_lifecycle"), "outcome": attribute.StringValue(string(input.Outcome)), "code": attribute.StringValue(input.Code), "support_reference": attribute.StringValue(reference), "retry": attribute.StringValue("PRIVATE_RETRY"), "route_id": attribute.StringValue("PRIVATE_ROUTE")}
		log := sanitizeLog(&sentry.Log{Attributes: attrs}, "paperboat-tunnel", "route-test", "production")
		if log == nil || log.Attributes["retry"].AsString() != string(input.Retry) || log.Attributes["route_id"].AsString() != "" {
			t.Fatalf("unsafe/lost route projection for %s", kind)
		}
		if input.Severity == "debug" && log.Level != sentry.LogLevelDebug || input.Severity == "warn" && log.Level != sentry.LogLevelWarn {
			t.Fatalf("severity lost for %s", kind)
		}
	}
	reporter.Observe(t.Context(), "route_lifecycle", "success", "PRIVATE_CODE", reference, -1)
	reporter.Close()
	mu.Lock()
	body := string(payload)
	mu.Unlock()
	for _, kind := range kinds {
		if !strings.Contains(body, string(kind)) {
			t.Fatalf("actual HTTP export missing %s", kind)
		}
	}
	if strings.Contains(body, "PRIVATE") || !strings.Contains(body, reference) || !strings.Contains(body, "wait_for_change") {
		t.Fatal("actual HTTP projection lost correlation/retry or leaked private input")
	}
	if reporter.SDKHTTPFailures() != 0 || reporter.FlushStatus() != "drained" {
		t.Fatal("healthy HTTP export did not drain")
	}
}
