package errorreport

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/getsentry/sentry-go"
	"github.com/getsentry/sentry-go/attribute"
	"github.com/pinksaucepasta/paperboat/internal/supportref"
)

func TestAllSignalEnvelopesAndPrivacy(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		mu.Unlock()
	}))
	defer server.Close()
	transport := sentry.NewHTTPTransport()
	transport.BufferSize = 8
	reporter := newSignalReporter(strings.Replace(server.URL, "http://", "http://public@", 1)+"/1", "test-release", transport, true, true, 1)
	defer reporter.Flush(context.Background())
	ctx := supportref.WithContext(context.Background(), "pb-0123456789abcdef0123456789abcdef")
	ctx, end := reporter.Start(ctx, "paperboat-cli", "login")
	reporter.Capture(ctx, "paperboat-cli", "unexpected_failure")
	logger := sentry.NewLogger(reporter.context(ctx))
	logger.SetAttributes(attribute.String("password", "PRIVATE-MARKER"))
	logger.Info().Emit("paperboat.operation")
	reporter.RegisterMetrics(func() []MetricSample {
		return []MetricSample{{Name: "paperboat_runtime_restart_total_snapshot", Value: 7}}
	}, []MetricDescriptor{{Name: "paperboat_runtime_restart_total_snapshot"}})
	end("success")
	reporter.Flush(context.Background())
	mu.Lock()
	defer mu.Unlock()
	joined := strings.Join(bodies, "\n")
	for _, kind := range []string{`"type":"event"`, `"type":"transaction"`, `"type":"log"`, `"type":"trace_metric"`} {
		if !strings.Contains(joined, kind) {
			t.Errorf("missing envelope %s in %s", kind, joined)
		}
	}
	for _, secret := range []string{"PRIVATE-MARKER", "sentry.server.address", "password", "os.name"} {
		if strings.Contains(joined, secret) {
			t.Errorf("private field exported: %s", secret)
		}
	}
	if !strings.Contains(joined, "paperboat_runtime_restart_total_snapshot") {
		t.Fatal("final cumulative snapshot missing")
	}
}
func TestOwnedHTTPTraceParentAndForeignOrigin(t *testing.T) {
	transport := &sentry.MockTransport{}
	r := newSignalReporter("https://public@example.invalid/1", "test", transport, true, true, 1)
	defer r.Flush(context.Background())
	restore := Install(r)
	defer restore()
	ctx, end := r.Start(context.Background(), "paperboat-cli", "login")
	defer end("success")
	parent := sentry.SpanFromContext(ctx)
	var header string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		header = req.Header.Get("sentry-trace")
		if req.Header.Get("baggage") != "" {
			t.Error("baggage exported")
		}
	}))
	defer server.Close()
	client := &http.Client{Transport: Transport(nil, server.URL)}
	req, _ := http.NewRequestWithContext(ctx, "GET", server.URL+"/private-project-name", nil)
	req.Header.Set("baggage", "secret")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if !strings.HasPrefix(header, parent.TraceID.String()+"-") || strings.Contains(header, parent.SpanID.String()) {
		t.Fatal("outbound child did not preserve trace with its own span")
	}
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Header.Get("sentry-trace") != "" || req.Header.Get("baggage") != "" {
			t.Error("trace leaked to upload host")
		}
	}))
	defer other.Close()
	req, _ = http.NewRequestWithContext(ctx, "GET", other.URL, nil)
	req.Header.Set("sentry-trace", header)
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
}
func TestSignalBoundsDisableAndConcurrentShutdown(t *testing.T) {
	r := newSignalReporter("https://public@example.invalid/1", "test", &sentry.MockTransport{}, true, true, 1)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				r.Observe(context.Background(), "paperboat-daemon", "daemon", "success", time.Millisecond)
			}
		}()
	}
	wg.Wait()
	if r.Dropped()[0] == 0 || r.Dropped()[1] == 0 {
		t.Fatal("rate budgets did not bound signals")
	}
	wg.Add(2)
	for i := 0; i < 2; i++ {
		go func() { defer wg.Done(); r.Flush(context.Background()) }()
	}
	wg.Wait()
	if r.Enabled() {
		t.Fatal("reporter stayed enabled")
	}
	disabled := newSignalReporter("https://public@example.invalid/1", "test", &sentry.MockTransport{}, false, false, 0)
	defer disabled.Flush(context.Background())
	ctx, end := disabled.Start(context.Background(), "paperboat-cli", "status")
	end("success")
	if sentry.SpanFromContext(ctx) != nil {
		t.Fatal("tracing enabled while disabled")
	}
}

func TestStrictSignalFlags(t *testing.T) {
	t.Setenv("PB_SENTRY_ENABLED", "true")
	t.Setenv("PB_SENTRY_DSN", "https://public@example.invalid/1")
	t.Setenv("PB_SENTRY_RELEASE", "test")
	for _, key := range []string{"PB_SENTRY_LOGS_ENABLED", "PB_SENTRY_METRICS_ENABLED"} {
		for _, value := range []string{"FALSE", "yes", "0", " true "} {
			t.Setenv(key, value)
			r := FromEnvironment()
			if r.Enabled() {
				r.Flush(context.Background())
				t.Fatalf("%s=%q enabled exporter", key, value)
			}
		}
		t.Setenv(key, "")
	}
	t.Setenv("PB_SENTRY_LOGS_ENABLED", "false")
	t.Setenv("PB_SENTRY_METRICS_ENABLED", "true")
	r := FromEnvironment()
	defer r.Flush(context.Background())
	if r.logs || !r.metrics {
		t.Fatal("independent flags ignored")
	}
}
func TestSnapshotBudgetRotatesWithoutChangingCumulativeValues(t *testing.T) {
	transport := &sentry.MockTransport{}
	r := newSignalReporter("https://public@example.invalid/1", "test", transport, false, true, 0)
	defer r.Flush(context.Background())
	samples := make([]MetricSample, snapshotLimit+16)
	values := map[string]bool{}
	for i := range samples {
		value := fmt.Sprint(i)
		values[value] = true
		samples[i] = MetricSample{Name: "paperboat_runtime_test_snapshot", Value: float64(i + 1), Labels: map[string]string{"kind": value}}
	}
	r.RegisterMetrics(func() []MetricSample { return samples }, []MetricDescriptor{{Name: "paperboat_runtime_test_snapshot", Labels: map[string]map[string]bool{"kind": values}}})
	r.sampleMetrics()
	r.client.Flush(time.Second)
	count := 0
	for _, e := range transport.Events() {
		count += len(e.Metrics)
	}
	if count != snapshotLimit || r.SnapshotDropped() != 16 {
		t.Fatalf("count=%d dropped=%d", count, r.SnapshotDropped())
	}
	r.sampleMetrics()
	r.client.Flush(time.Second)
	seen := map[string]bool{}
	for _, e := range transport.Events() {
		for _, m := range e.Metrics {
			key := m.Attributes["dimension.kind"].AsString()
			seen[key] = true
			expected, _ := strconv.Atoi(key)
			if value, ok := m.Value.Float64(); !ok || value != float64(expected+1) {
				t.Fatal("cumulative snapshot value changed")
			}
		}
	}
	if len(seen) != len(samples) {
		t.Fatalf("rotation starved %d series", len(samples)-len(seen))
	}
}
func TestTransactionContextAndLifecycleRejectUnknownValues(t *testing.T) {
	var trace sentry.TraceID
	trace[0] = 1
	var span sentry.SpanID
	span[0] = 1
	event := sanitizeTransaction(&sentry.Event{Contexts: map[string]sentry.Context{"trace": {"trace_id": trace, "span_id": span, "op": "PRIVATE-MARKER", "status": "PRIVATE-MARKER", "private": "PRIVATE-MARKER"}}}, nil)
	raw, _ := json.Marshal(event)
	if strings.Contains(string(raw), "PRIVATE-MARKER") {
		t.Fatal("untrusted trace context exported")
	}
	transport := &sentry.MockTransport{}
	r := newSignalReporter("https://public@example.invalid/1", "test", transport, true, false, 0)
	defer r.Flush(context.Background())
	r.Lifecycle(context.Background(), "service", "component_start", "start_failed", "failed")
	r.Lifecycle(context.Background(), "service", "secret_operation", "secret_code", "state_change")
	r.Flush(context.Background())
	found := false
	for _, e := range transport.Events() {
		for _, l := range e.Logs {
			if l.Attributes["operation"].AsString() == "component_start" && l.Attributes["code"].AsString() == "start_failed" {
				found = true
			}
			b, _ := json.Marshal(l)
			if strings.Contains(string(b), "secret_") {
				t.Fatal("unknown lifecycle label exported")
			}
		}
	}
	if !found {
		t.Fatal("known stage/code lost")
	}
}

type referenceRoundTripper func(*http.Request) (*http.Response, error)

func (f referenceRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestOwnedReferenceIndependentOfReportingAndHeaderWins(t *testing.T) {
	const contextRef = "pb-0123456789abcdef0123456789abcdef"
	const headerRef = "pb-fedcba9876543210fedcba9876543210"
	for _, test := range []struct {
		name                     string
		enabled                  bool
		contextRef, header, want string
	}{
		{"disabled_context", false, contextRef, "", contextRef}, {"disabled_header", false, contextRef, headerRef, headerRef}, {"disabled_invalid_header", false, contextRef, "bad", contextRef}, {"disabled_fresh", false, "", "", ""}, {"enabled_header", true, contextRef, headerRef, headerRef},
	} {
		t.Run(test.name, func(t *testing.T) {
			reporter := &Reporter{}
			if test.enabled {
				reporter = newSignalReporter("https://public@example.invalid/1", "test", &sentry.MockTransport{}, true, true, 1)
			}
			defer reporter.Flush(context.Background())
			restore := Install(reporter)
			defer restore()
			ctx := supportref.WithContext(context.Background(), test.contextRef)
			ctx, finish := reporter.Start(ctx, "paperboat-cli", "status")
			defer finish("success")
			transport := Transport(referenceRoundTripper(func(req *http.Request) (*http.Response, error) {
				ref := req.Header.Get(supportref.Header)
				if !supportref.Valid(ref) || (test.want != "" && ref != test.want) || supportref.FromContext(req.Context()) != ref {
					t.Fatal("request reference/context disagree")
				}
				if span := sentry.SpanFromContext(req.Context()); span != nil && span.Tags["support_reference"] != ref {
					t.Fatal("span/server reference disagree")
				}
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
			}), "https://control.example")
			req, _ := http.NewRequestWithContext(ctx, "GET", "https://control.example/v1/status", nil)
			req.Header.Set(supportref.Header, test.header)
			resp, err := transport.RoundTrip(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
		})
	}
}
