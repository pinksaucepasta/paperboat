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
	"syscall"
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
		{name: "official default on", defaultDSN: dsn, defaultRelease: "paperboat-relay:1", wantEnabled: true},
		{name: "explicit disable", enabled: "false", defaultDSN: dsn, defaultRelease: "paperboat-relay:1"},
		{name: "environment overrides embedded", envDSN: "https://override@example.invalid/2", envRelease: "override:2", defaultDSN: dsn, defaultRelease: "paperboat-relay:1", wantEnabled: true},
		{name: "partial configuration invalid", defaultDSN: dsn, wantError: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("PAPERBOAT_SENTRY_ENABLED", test.enabled)
			t.Setenv("PAPERBOAT_SENTRY_DSN", test.envDSN)
			t.Setenv("PAPERBOAT_SENTRY_RELEASE", test.envRelease)
			DefaultDSN, DefaultRelease = test.defaultDSN, test.defaultRelease
			reporter, err := New("paperboat-relay")
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
	client, err := sentry.NewClient(options("paperboat-relay", "release", "https://public@example.invalid/1", &recordingTransport{}))
	if err != nil {
		t.Fatal(err)
	}
	reporter := &Reporter{client: client, hub: sentry.NewHub(client, sentry.NewScope()), enabled: true, traces: true, component: "paperboat-relay"}
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
	if _, err := New("paperboat-relay"); err == nil {
		t.Fatal("missing DSN and release accepted")
	}
}

func TestEnabledConfigurationAcceptsQualifiedRelease(t *testing.T) {
	t.Setenv("PAPERBOAT_SENTRY_ENABLED", "true")
	t.Setenv("PAPERBOAT_SENTRY_DSN", "https://public@example.invalid/1")
	t.Setenv("PAPERBOAT_SENTRY_RELEASE", "paperboat-relay:validation-20260919")
	t.Setenv("PAPERBOAT_SENTRY_REGION", "validation")
	t.Setenv("PAPERBOAT_SENTRY_INSTANCE", "slot-01")
	reporter, err := New("paperboat-relay")
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
		if _, err := New("paperboat-relay"); err == nil {
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
	client, err := sentry.NewClient(options("paperboat-relay", "2026.09.19.1", "https://public@example.invalid/1", transport))
	if err != nil {
		t.Fatal(err)
	}
	const ref = "support_01234567-89ab-4def-8123-456789abcdef"
	reporter := &Reporter{client: client, enabled: true, component: "paperboat-relay", localFault: func(Fault) {}}
	for i := 0; i < maxEvents+3; i++ {
		reporter.CaptureFailure(WithSupportReference(context.Background(), ref), "service_run", errors.New("secret=value /private/path"))
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
	if event.Tags["support_reference"] != ref || event.Tags["correlation_id"] != ref || event.Tags["component"] != "paperboat-relay" {
		t.Fatalf("tags=%v", event.Tags)
	}
	if event.Message != "paperboat service failure" || event.Release != "2026.09.19.1" {
		t.Fatalf("message=%q release=%q", event.Message, event.Release)
	}
	if event.Tags["stage"] != "serve" || event.Tags["code"] != "service_failed" || event.Tags["cause"] != "internal" || event.Tags["error_type"] != "errorString" || event.Tags["outcome"] != "failed" {
		t.Fatalf("fault tags=%v", event.Tags)
	}
	if event.Tags["error_chain"] == "" || event.Tags["source_file"] != "reporting_test.go" || event.Tags["source_function"] == "" || event.Tags["source_line"] == "" {
		t.Fatalf("missing safe fault metadata: %v", event.Tags)
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
	ref := "support_01234567-89ab-4def-8123-456789abcdef"
	dirty := &sentry.Event{
		Message: "token=secret", Logger: "private", Transaction: "/users/alice", ServerName: "host",
		Environment: "private", Request: &sentry.Request{URL: "https://secret.invalid/path"},
		Contexts: map[string]sentry.Context{"secret": {"value": "private"}},
		Tags:     map[string]string{"support_reference": ref, "operation": "service_lifecycle", "stage": "serve", "code": "service_failed", "cause": "internal", "error_type": "error", "outcome": "failed", "secret": "private"},
		Exception: []sentry.Exception{{Value: "password", Stacktrace: &sentry.Stacktrace{Frames: []sentry.Frame{{
			Function: "github.com/private/project/internal/service.run", Filename: "/home/alice/private/service.go",
			AbsPath: "/home/alice/private/service.go", Vars: map[string]any{"token": "secret"}, Lineno: 42,
		}}}}},
	}
	clean := sanitize(dirty, "paperboat-relay", "2026.09.19.1")
	if clean.Request != nil || clean.Contexts != nil || clean.Logger != "" || clean.Transaction != "" || clean.ServerName != "" || clean.Environment != "" {
		t.Fatalf("unsafe fields survived: %+v", clean)
	}
	if clean.Tags["support_reference"] != ref || clean.Tags["cause"] != "internal" || clean.Tags["operation"] != "service_lifecycle" || clean.Tags["secret"] != "" {
		t.Fatalf("fault allowlist=%v", clean.Tags)
	}
	frame := clean.Exception[0].Stacktrace.Frames[0]
	if frame.Filename != "service.go" || frame.Module != "service" || frame.Function != "run" || frame.Lineno != 42 || frame.AbsPath != "" || frame.Vars != nil {
		t.Fatalf("frame=%+v", frame)
	}
}

func TestReferenceFormat(t *testing.T) {
	seen := map[string]bool{}
	for range 2 {
		reference := Reference()
		if !validReference(reference) || seen[reference] {
			t.Fatalf("invalid or reused reference %q", reference)
		}
		seen[reference] = true
	}
	for _, reference := range []string{"", "pb-0123456789abcdef0123456789abcdef", "support_01234567-89ab-1def-8123-456789abcdef", "support_01234567-89ab-4def-0123-456789abcdef", "support_01234567-89AB-4def-8123-456789abcdef"} {
		if validReference(reference) {
			t.Errorf("accepted invalid reference %q", reference)
		}
	}
}

func TestCaptureFailureKeepsSafeMetadataWhenSentryIsDisabled(t *testing.T) {
	var local []Fault
	reporter := &Reporter{localFault: func(fault Fault) { local = append(local, fault) }}
	const reference = "support_01234567-89ab-4def-8123-456789abcdef"
	err := errors.Join(errors.New("token=private https://private.example/path"), context.DeadlineExceeded)
	fault := reporter.CaptureFailure(WithSupportReference(context.Background(), reference), "service_run", err)
	if len(local) != 1 || fault.CorrelationID != reference || local[0].CorrelationID != reference {
		t.Fatalf("fault=%+v local=%+v", fault, local)
	}
	if fault.Schema != "paperboat.edge_event.v1" || fault.Stage != "serve" || fault.Code != "service_failed" || fault.Cause != "deadline_exceeded" || fault.Outcome != "failed" || fault.ErrorType == "" || len(fault.ErrorChain) == 0 || fault.SupportReference != reference || fault.CorrelationID != reference || fault.SourceFile == "" || fault.SourceFunction == "" || fault.SourceLine <= 0 {
		t.Fatalf("incomplete fault projection: %+v", fault)
	}
	raw, err := json.Marshal(local[0])
	if err != nil || strings.Contains(string(raw), "private") || strings.Contains(string(raw), "example") || strings.Contains(string(raw), "token=") {
		t.Fatalf("local fault leaked private error content: %s err=%v", raw, err)
	}
}

func TestConfigurationFailureIsLocalRejection(t *testing.T) {
	var local Fault
	reporter := &Reporter{localFault: func(fault Fault) { local = fault }}
	fault := reporter.CaptureFailure(context.Background(), "service_config", errors.New("private config path and body"))
	if fault.Stage != "configure" || fault.Code != "service_setup_failed" || fault.Outcome != "rejected" || fault.Severity != "warning" || fault.CorrelationID == "" || local.CorrelationID != fault.CorrelationID {
		t.Fatalf("configuration fault=%+v local=%+v", fault, local)
	}
}

type statusFailure struct{ status int }

func (*statusFailure) Error() string           { return "token=private /customer/path" }
func (e *statusFailure) DiagnosticStatus() int { return e.status }

func TestFaultProjectionAddsTypedCauseMetadata(t *testing.T) {
	var local Fault
	reporter := &Reporter{localFault: func(fault Fault) { local = fault }}
	const reference = "support_01234567-89ab-4def-8123-456789abcdef"
	status := reporter.CaptureFailure(WithSupportReference(context.Background(), reference), "service_run", &statusFailure{status: 503})
	if status.Cause != "service_unavailable" || status.HTTPStatus != 503 || status.Outcome != "failed" || local.SupportReference != reference || local.CorrelationID != reference {
		t.Fatalf("status fault=%+v local=%+v", status, local)
	}
	rejected := reporter.CaptureFailure(WithSupportReference(context.Background(), reference), "service_run", &statusFailure{status: http.StatusForbidden})
	if rejected.Cause != "permission_denied" || rejected.HTTPStatus != http.StatusForbidden || rejected.Outcome != "rejected" || rejected.Severity != "warning" || local.Outcome != "rejected" || local.Severity != "warning" {
		t.Fatalf("rejected fault=%+v local=%+v", rejected, local)
	}
	errno := reporter.CaptureFailure(WithSupportReference(context.Background(), reference), "service_run", syscall.ECONNREFUSED)
	if errno.Cause != "connection_refused" || errno.Errno != int(syscall.ECONNREFUSED) || len(errno.ErrorChain) == 0 {
		t.Fatalf("errno fault=%+v", errno)
	}
}

func TestCanceledControlTraceIsNotReportedAsSuccess(t *testing.T) {
	transport := &recordingTransport{}
	config := options("paperboat-relay", "release", "https://public@example.invalid/1", transport)
	config.EnableTracing, config.TracesSampleRate = true, 1
	client, err := sentry.NewClient(config)
	if err != nil {
		t.Fatal(err)
	}
	reporter := &Reporter{client: client, hub: sentry.NewHub(client, sentry.NewScope()), enabled: true, traces: true, component: "paperboat-relay"}
	_, _, finish := reporter.ControlTrace(context.Background(), "dependency_health")
	finish("canceled", "shutdown")
	reporter.Close()
	var canceled bool
	for _, event := range transport.events {
		if event.Type != "transaction" {
			continue
		}
		if event.Contexts["trace"]["status"] != "cancelled" {
			t.Fatalf("canceled operation status=%v", event.Contexts["trace"]["status"])
		}
		canceled = true
	}
	if !canceled {
		t.Fatal("canceled control operation emitted no trace")
	}
}

func TestCanceledFailureHasNoSentryException(t *testing.T) {
	transport := &recordingTransport{}
	config := options("paperboat-relay", "release", "https://public@example.invalid/1", transport)
	client, err := sentry.NewClient(config)
	if err != nil {
		t.Fatal(err)
	}
	var local []Fault
	reporter := &Reporter{client: client, hub: sentry.NewHub(client, sentry.NewScope()), enabled: true, logs: true, metrics: true, component: "paperboat-relay", localFault: func(fault Fault) { local = append(local, fault) }}
	const reference = "support_01234567-89ab-4def-8123-456789abcdef"
	fault := reporter.CaptureFailure(WithSupportReference(context.Background(), reference), "service_run", context.Canceled)
	reporter.Close()
	if fault.Outcome != "canceled" || fault.Severity != "info" || fault.Cause != "context_canceled" || len(local) != 1 || local[0].CorrelationID != reference {
		t.Fatalf("fault=%+v local=%+v", fault, local)
	}
	for _, event := range transport.events {
		if len(event.Exception) != 0 {
			t.Fatalf("cancellation was sent as exception: %+v", event.Exception)
		}
		for _, log := range event.Logs {
			if log.Attributes["outcome"].AsInterface() == "canceled" && log.Level != sentry.LogLevelInfo {
				t.Fatalf("cancellation log level=%s", log.Level)
			}
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
	reporter, err := New("paperboat-relay")
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
	if _, err := New("paperboat-relay"); err == nil {
		t.Fatal("invalid trace sample rate accepted")
	}
}

func TestOperationEmitsLogMetricAndTraceEnvelopes(t *testing.T) {
	transport := &recordingTransport{}
	config := options("paperboat-relay", "2026.09.19.1", "https://public@example.invalid/1", transport)
	config.EnableTracing = true
	config.TracesSampleRate = 1
	config.BeforeSend = func(event *sentry.Event, _ *sentry.EventHint) *sentry.Event {
		return sanitizeEnvironment(event, "paperboat-relay", "2026.09.19.1", "production", "in-blr", "slot-01")
	}
	config.BeforeSendLog = func(log *sentry.Log) *sentry.Log {
		return sanitizeLog(log, "paperboat-relay", "2026.09.19.1", "production", "in-blr", "slot-01")
	}
	config.BeforeSendMetric = func(metric *sentry.Metric) *sentry.Metric {
		return sanitizeMetric(metric, "paperboat-relay", "2026.09.19.1", "production", "in-blr", "slot-01")
	}
	config.BeforeSendTransaction = func(event *sentry.Event, _ *sentry.EventHint) *sentry.Event {
		return sanitizeTransaction(event, "paperboat-relay", "2026.09.19.1", "production", "in-blr", "slot-01")
	}
	client, err := sentry.NewClient(config)
	if err != nil {
		t.Fatal(err)
	}
	reporter := &Reporter{client: client, hub: sentry.NewHub(client, sentry.NewScope()), enabled: true, logs: true, traces: true, metrics: true, component: "paperboat-relay"}
	const reference = "support_01234567-89ab-4def-8123-456789abcdef"
	reporter.Observe(context.Background(), "relay_admission", "rejected", "unauthorized", reference, time.Second)
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
			if log.Body != "paperboat.operation" || log.Attributes["operation"].AsInterface() != "relay_admission" {
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
	client, err := sentry.NewClient(options("paperboat-relay", "2026.09.19.1", "https://public@example.invalid/1", transport))
	if err != nil {
		t.Fatal(err)
	}
	reporter := &Reporter{client: client, hub: sentry.NewHub(client, sentry.NewScope()), enabled: true, logs: true, component: "paperboat-relay"}
	for range 129 {
		reporter.Observe(context.Background(), "relay_session", "success", "ok", "", 0)
	}
	if reporter.droppedLogs.Load() != 1 {
		t.Fatalf("drops=%d", reporter.droppedLogs.Load())
	}
	reporter.window.Store(reporter.window.Load() - 1)
	reporter.Observe(context.Background(), "relay_session", "success", "recovered", "", 0)
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
	clientOptions := options("paperboat-relay", "release", dsn, transport)
	client, err := sentry.NewClient(clientOptions)
	if err != nil {
		t.Fatal(err)
	}
	reporter := &Reporter{client: client, hub: sentry.NewHub(client, sentry.NewScope()), enabled: true, metrics: true, component: "paperboat-relay"}
	for range maxDetailMetrics/2 + 1 {
		reporter.Observe(context.Background(), "relay_session", "success", "ok", "", 0)
	}
	snapshots := make([]Snapshot, maxSnapshots+1)
	snapshots[0] = Snapshot{Name: "paperboat_relay_sessions", Kind: "gauge", Value: 30}
	snapshots[1] = Snapshot{Name: "paperboat_relay_capacity_limit", Kind: "gauge", Value: 256}
	for i := 2; i < maxSnapshots; i++ {
		snapshots[i] = Snapshot{Name: "paperboat_relay_sessions", Kind: "gauge", Value: float64(i)}
	}
	snapshots[maxSnapshots] = Snapshot{Name: "paperboat_peer_relay_allocations", Kind: "gauge", Value: 12}
	// The exact production SDK path has a 100-item queue. This window
	// contains 28 detailed items, eight health gauges and 64 snapshots.
	reporter.ExportDrops(context.Background())
	reporter.MetricSnapshots(context.Background(), snapshots)
	if !client.Flush(flushTimeout) {
		t.Fatal("first snapshot window did not flush")
	}
	mu.Lock()
	firstWindow := bytes.Join(envelopes, nil)
	mu.Unlock()
	for _, name := range []string{"paperboat_relay_sessions", "paperboat_relay_capacity_limit", "paperboat_reporting_metrics_dropped_total_snapshot"} {
		if !bytes.Contains(firstWindow, []byte(name)) {
			t.Fatalf("health snapshot missing after production drain: %s", name)
		}
	}
	if count := metricItemCount(t, firstWindow); count != 100 {
		t.Fatalf("first-window metric items=%d, want 100", count)
	}
	// A completed first window establishes its real HTTP receipt before
	// testing the next scheduled snapshot rotation.
	reporter.ExportDrops(context.Background())
	reporter.MetricSnapshots(context.Background(), snapshots)
	reporter.Close()
	if reporter.snapshotCursor.Load() != 2*maxSnapshots {
		t.Fatalf("cursor=%d", reporter.snapshotCursor.Load())
	}
	waitForEnvelopeContent(t, &mu, &envelopes, notifications, "paperboat_peer_relay_allocations")
}

func TestSanitizersRejectMaliciousTraceAndMetricFields(t *testing.T) {
	ref := "support_01234567-89ab-4def-8123-456789abcdef"
	event := &sentry.Event{Transaction: "relay_session", Tags: map[string]string{"outcome": "failed", "code": "internal", "support_reference": ref, "secret": "credential"}, Contexts: map[string]sentry.Context{"trace": {"trace_id": "0123456789abcdef0123456789abcdef", "span_id": "0123456789abcdef", "parent_span_id": "fedcba9876543210", "op": "secret/path", "description": "token=secret", "data": map[string]any{"key": "secret"}}, "request": {"url": "https://secret.invalid"}}}
	clean := sanitizeTransaction(event, "paperboat-relay", "release", "production")
	if clean == nil {
		t.Fatal("valid trace rejected")
	}
	trace := clean.Contexts["trace"]
	if len(trace) != 2 || trace["op"] != "paperboat.relay_session" || trace["description"] != nil || clean.Contexts["request"] != nil || clean.Tags["secret"] != "" || clean.Tags["support_reference"] != ref {
		t.Fatalf("unsafe transaction=%+v", clean)
	}
	malicious := &sentry.Metric{Name: "paperboat_relay_SECRET", Type: sentry.MetricTypeGauge, Attributes: map[string]attribute.Value{"secret": attribute.StringValue("credential")}}
	if sanitizeMetric(malicious, "paperboat-relay", "release", "production") != nil {
		t.Fatal("unknown prefixed metric accepted")
	}
	wrongEnum := &sentry.Metric{Name: "paperboat_relay_admissions_total_snapshot", Type: sentry.MetricTypeGauge, Attributes: map[string]attribute.Value{"outcome": attribute.StringValue("account_123")}}
	if sanitizeMetric(wrongEnum, "paperboat-relay", "release", "production") != nil {
		t.Fatal("unknown label enum accepted")
	}
}

func TestErrorRateLimitRecoversAndCloseIsIdempotent(t *testing.T) {
	transport := &recordingTransport{}
	client, err := sentry.NewClient(options("paperboat-relay", "release", "https://public@example.invalid/1", transport))
	if err != nil {
		t.Fatal(err)
	}
	reporter := &Reporter{client: client, hub: sentry.NewHub(client, sentry.NewScope()), enabled: true, component: "paperboat-relay", localFault: func(Fault) {}}
	const ref = "support_01234567-89ab-4def-8123-456789abcdef"
	for range maxEvents + 1 {
		reporter.CaptureFailure(WithSupportReference(context.Background(), ref), "service_run", errors.New("private cause"))
	}
	if reporter.droppedErrors.Load() != 1 {
		t.Fatalf("drops=%d", reporter.droppedErrors.Load())
	}
	reporter.window.Store(reporter.window.Load() - 1)
	reporter.CaptureFailure(WithSupportReference(context.Background(), ref), "service_run", errors.New("private cause"))
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
	config := options("paperboat-relay", "release", "https://public@example.invalid/1", transport)
	client, err := sentry.NewClient(config)
	if err != nil {
		t.Fatal(err)
	}
	reporter := &Reporter{client: client, hub: sentry.NewHub(client, sentry.NewScope()), enabled: true, logs: true, metrics: true, component: "paperboat-relay", localFault: func(f Fault) { local = append(local, f) }}
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

func TestSelfhostFaultUsesFiniteComponentAndPreservesProcessReference(t *testing.T) {
	transport := &recordingTransport{}
	client, err := sentry.NewClient(options("paperboat-selfhost", "release", "https://public@example.invalid/1", transport))
	if err != nil {
		t.Fatal(err)
	}
	var local []Fault
	reporter := &Reporter{client: client, hub: sentry.NewHub(client, sentry.NewScope()), enabled: true, logs: true, metrics: true, component: "paperboat-selfhost", localFault: func(f Fault) { local = append(local, f) }}
	reference := Reference()
	ctx := WithSupportReference(context.Background(), reference)
	reporter.ObserveFailure(ctx, "selfhost_child", syscall.ECONNREFUSED)
	reporter.CaptureFailure(ctx, "selfhost_command", syscall.EACCES)
	reporter.Close()
	logs, exceptions := 0, 0
	for _, event := range transport.events {
		if len(event.Exception) > 0 {
			exceptions++
			if event.Tags["component"] != "paperboat-selfhost" || event.Tags["support_reference"] != reference || event.Tags["code"] != "selfhost_command_failed" {
				t.Fatal("selfhost exception projection mismatch")
			}
		}
		for _, log := range event.Logs {
			logs++
			if log.Attributes["component"].AsInterface() != "paperboat-selfhost" || log.Attributes["support_reference"].AsInterface() != reference {
				t.Fatal("selfhost log projection mismatch")
			}
		}
	}
	if len(local) != 2 || logs != 2 || exceptions != 1 {
		t.Fatalf("local=%d logs=%d exceptions=%d", len(local), logs, exceptions)
	}
}
