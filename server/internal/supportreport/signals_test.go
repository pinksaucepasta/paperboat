package supportreport

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/getsentry/sentry-go"
	"github.com/getsentry/sentry-go/attribute"
	serverdiagnostics "github.com/pinksaucepasta/paperboat-server/internal/diagnostics"
	"github.com/pinksaucepasta/paperboat-server/internal/telemetry"
)

func TestAllSignalEnvelopesAndOwnedTraceCorrelation(t *testing.T) {
	var mu sync.Mutex
	var envelopes []string
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		envelopes = append(envelopes, string(b))
		mu.Unlock()
		w.WriteHeader(200)
	}))
	defer sink.Close()
	rate := 1.0
	s, err := New(&memoryStore{}, Config{Enabled: true, DSN: strings.Replace(sink.URL, "://", "://public@", 1) + "/1", Release: "server-test", Environment: "test", Region: "mumbai", Instance: "slot-01", TracesSampleRate: &rate})
	if err != nil {
		t.Fatal(err)
	}
	defer s.client.Close()
	ref := NewReference()
	parent := "123456789012345678901234567890ab-12345678901234ab-1"
	ctx, finish := s.Begin(context.Background(), "http_billing", ref, parent)
	span := sentry.SpanFromContext(ctx)
	if span == nil || span.TraceID.String() != strings.Split(parent, "-")[0] || span.ParentSpanID.String() != "12345678901234ab" {
		t.Fatalf("incoming parent was not preserved: span=%+v", span)
	}
	span.SetData("request", "SECRET_REQUEST")
	sentry.GetHubFromContext(ctx).Scope().SetUser(sentry.User{Email: "SECRET_EMAIL"})
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("sentry-trace") != "" || r.Header.Get("baggage") != "" {
			t.Error("propagated trace into external provider")
		}
		w.WriteHeader(503)
	}))
	defer provider.Close()
	req, _ := http.NewRequestWithContext(ctx, "GET", provider.URL+"/SECRET_PATH?key=SECRET_KEY", nil)
	response, err := serverdiagnostics.InstrumentProviderTransport("stripe", http.DefaultTransport).RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	s.CaptureContext(ctx, ref, "user_secret", "request_a", "handler", "internal_error", true, sentry.NewStacktrace())
	s.process(ctx, <-s.queue)
	finish("failed")
	registry := telemetry.NewMetrics()
	registry.AddCounter(telemetry.MetricHTTPRequests, telemetry.MetricLabels{"method": "post", "route_family": "public_api", "status_class": "5xx"}, 17)
	registry.ObserveHistogram(telemetry.MetricHTTPDuration, telemetry.MetricLabels{"route_family": "public_api", "status_class": "5xx"}, .2)
	s.RegistrySnapshot(registry.Snapshot())
	age := 12.0
	s.WorkerProgress("worker_machines", 3, &age)
	s.WorkerProgress("PRIVATE_WORKER", 3, &age)
	s.Gauge("paperboat.backup.state", 3)
	s.Gauge("paperboat.restore.state", 1)
	s.Gauge("paperboat.backup.age_seconds", 123)
	s.Gauge("paperboat.restore.age_seconds", 456)
	sentry.NewLogger(ctx).Info().String("operation", "http_billing").String("outcome", "success").String("secret", "SECRET_LOG").Emit("paperboat.operation")
	sentry.NewMeter(ctx).Count("paperboat.operation.count", 1, sentry.WithAttributes(attribute.String("operation", "http_billing"), attribute.String("outcome", "success"), attribute.String("secret", "SECRET_METRIC")))
	if !s.client.Flush(2 * time.Second) {
		t.Fatal("flush failed")
	}
	mu.Lock()
	payload := strings.Join(envelopes, "\n")
	for _, envelope := range envelopes {
		for _, typ := range []string{`"type":"event"`, `"type":"transaction"`, `"type":"log"`, `"type":"trace_metric"`} {
			if strings.Contains(envelope, typ) && (!strings.Contains(envelope, `"mumbai"`) || !strings.Contains(envelope, `"slot-01"`)) {
				t.Errorf("%s missing configured fleet identity", typ)
			}
			if (typ == `"type":"log"` || typ == `"type":"trace_metric"`) && strings.Contains(envelope, typ) && (!strings.Contains(envelope, `"sentry.release"`) || !strings.Contains(envelope, `"sentry.environment"`)) {
				t.Errorf("%s missing configured SDK release/environment identity", typ)
			}
		}
	}
	mu.Unlock()
	for _, want := range []string{`"type":"event"`, `"type":"transaction"`, `"type":"log"`, `"type":"trace_metric"`, ref, "paperboat.worker.state", "paperboat.worker.progress_age_seconds", "worker_machines", "paperboat.backup.state", "paperboat.restore.state", "paperboat.backup.age_seconds", "paperboat.restore.age_seconds", `"region"`, `"mumbai"`, `"instance"`, `"slot-01"`, "http_billing", "provider_stripe", "paperboat.operation.duration", telemetry.MetricHTTPRequests + "_snapshot", "123456789012345678901234567890ab"} {
		if !strings.Contains(payload, want) {
			t.Errorf("missing %s in %s", want, payload)
		}
	}
	for _, secret := range []string{"PRIVATE_WORKER", "SECRET_", "user_secret", "server_name", "request_a", "sentry.server.address"} {
		if strings.Contains(payload, secret) {
			t.Errorf("export leaked %s", secret)
		}
	}
}

func TestSignalPrivacyBoundsAndDisabled(t *testing.T) {
	s, err := New(&memoryStore{}, Config{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, finish := s.Begin(context.Background(), "http_billing", NewReference(), "")
	finish("success")
	if sentry.SpanFromContext(ctx) != nil || s.SignalStatus()["enabled"] != false {
		t.Fatal("disabled reporting created telemetry")
	}
	rate := 0.0
	cfg := Config{Enabled: true, DSN: "https://public@example.invalid/1", Release: "test", Environment: "test", TracesSampleRate: &rate}
	s, err = New(&memoryStore{}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.client.Close()
	for i := 0; i < 605; i++ {
		s.admitSignal(0)
	}
	if s.signals.dropped[0].Load() != 5 {
		t.Fatal("log rate limit not enforced")
	}
	dirty := &sentry.Log{Body: "paperboat.operation", Attributes: map[string]attribute.Value{"operation": attribute.StringValue("http_auth"), "outcome": attribute.StringValue("success"), "support_reference": attribute.StringValue("SECRET"), "token": attribute.StringValue("SECRET")}}
	b, _ := json.Marshal(s.cleanLog(dirty))
	if strings.Contains(string(b), "SECRET") {
		t.Fatal("unsafe log")
	}
	dirty.Body = "SECRET"
	if s.cleanLog(dirty) != nil {
		t.Fatal("accepted arbitrary message")
	}
	metric := &sentry.Metric{Name: "paperboat.operation.count", Value: sentry.Int64MetricValue(1), Attributes: map[string]attribute.Value{"operation": attribute.StringValue("http_auth"), "outcome": attribute.StringValue("success"), "support_reference": attribute.StringValue(NewReference()), "host": attribute.StringValue("SECRET")}}
	metric.Attributes["sentry.release"] = attribute.StringValue("SECRET")
	metric.Attributes["sentry.environment"] = attribute.StringValue("SECRET")
	b, _ = json.Marshal(s.cleanMetric(metric))
	if strings.Contains(string(b), "SECRET") || strings.Contains(string(b), "support_reference") {
		t.Fatal("metric dimensions leaked")
	}
	metric.Name = "SECRET"
	if s.cleanMetric(metric) != nil {
		t.Fatal("accepted unregistered metric")
	}
	s.closed.Store(true)
	if s.admitSignal(0) {
		t.Fatal("admitted telemetry after shutdown")
	}
}

func TestLifecycleFailureRecoveryUsesSameLocalEvents(t *testing.T) {
	var mu sync.Mutex
	var logs []sentry.Log
	rate := 0.0
	s, err := New(&memoryStore{}, Config{Enabled: true, DSN: "https://public@example.invalid/1", Release: "test", Environment: "test", TracesSampleRate: &rate})
	if err != nil {
		t.Fatal(err)
	}
	defer s.client.Close()
	options := s.client.Options()
	options.Transport = sentry.NewHTTPTransport()
	options.BeforeSendLog = func(l *sentry.Log) *sentry.Log {
		clean := s.cleanLog(l)
		if clean != nil {
			mu.Lock()
			logs = append(logs, *clean)
			mu.Unlock()
		}
		return nil
	}
	client, err := sentry.NewClient(options)
	if err != nil {
		t.Fatal(err)
	}
	old := s.client
	s.client = client
	old.Close()
	defer client.Close()
	events, _ := telemetry.NewEventLog(8)
	defer events.Close()
	events.SetObserver(s.ObserveEvent)
	ref := NewReference()
	for _, outcome := range []telemetry.EventOutcome{telemetry.OutcomeFailed, telemetry.OutcomeSuccess} {
		_, err = events.Record(telemetry.EventInput{At: time.Now(), Severity: telemetry.SeverityInfo, Component: telemetry.DimensionService, Name: "connector_reconnect", Code: "connection_changed", Outcome: outcome, Message: "Connection state changed.", CorrelationID: ref, Retry: telemetry.RetryNone})
		if err != nil {
			t.Fatal(err)
		}
	}
	if err = events.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(logs) != 2 || logs[0].Attributes["outcome"].AsString() != "failed" || logs[1].Attributes["outcome"].AsString() != "success" {
		t.Fatalf("lost failure/recovery: %+v", logs)
	}
}
