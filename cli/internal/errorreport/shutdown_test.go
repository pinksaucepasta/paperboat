package errorreport

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/getsentry/sentry-go"
)

// Use the exact production transport/client constructor. Envelope responses
// prove HTTP handling here; SDK flush status alone is not receipt evidence.
func TestProductionTransportShutdownDrainsHealthySignals(t *testing.T) {
	var mu sync.Mutex
	var envelopes []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		data, _ := io.ReadAll(req.Body)
		mu.Lock()
		envelopes = append(envelopes, string(data))
		mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()
	transport := sentry.NewHTTPTransport()
	transport.BufferSize = queueCapacity
	transport.Timeout = flushLimit
	r := newSignalReporter(strings.Replace(server.URL, "http://", "http://public@", 1)+"/1", "test", transport, true, true, 1)
	if !r.client.Options().DisableTelemetryBuffer {
		t.Fatal("production constructor did not pin bounded legacy batch path")
	}
	ctx, finish := r.Start(context.Background(), "pb", "connect")
	r.CaptureFailure(ctx, "pb", "connect", "peer_connect", "transport_failed", syscall.ECONNREFUSED)
	r.Observe(ctx, "pb", "connect", "success", time.Millisecond)
	finish("success")
	r.RegisterMetrics(func() []MetricSample {
		return []MetricSample{{Name: "paperboat_runtime_restart_total_snapshot", Value: 1}}
	}, []MetricDescriptor{{Name: "paperboat_runtime_restart_total_snapshot"}})
	r.Flush(context.Background())
	if r.FlushStatus() != "drained" {
		t.Fatalf("shutdown=%s", r.FlushStatus())
	}
	if r.SDKHTTPFailures() != 0 {
		t.Fatal("healthy 202 response counted as failed")
	}
	mu.Lock()
	defer mu.Unlock()
	joined := strings.Join(envelopes, "\n")
	for _, kind := range []string{`"type":"event"`, `"type":"log"`, `"type":"transaction"`, `"type":"trace_metric"`, "paperboat_runtime_restart_total_snapshot"} {
		if !strings.Contains(joined, kind) {
			t.Fatalf("healthy accepted signal not received: %s", kind)
		}
	}
}

func TestProductionTransportFastExportFailuresRemainObservable(t *testing.T) {
	for _, networkFailure := range []bool{false, true} {
		t.Run(map[bool]string{false: "503", true: "network"}[networkFailure], func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(503) }))
			defer server.Close()
			dsn := strings.Replace(server.URL, "http://", "http://public@", 1) + "/1"
			if networkFailure {
				server.Close()
			}
			transport := sentry.NewHTTPTransport()
			transport.BufferSize, transport.Timeout = queueCapacity, flushLimit
			r := newSignalReporter(dsn, "test", transport, false, false, 0)
			r.Capture(context.Background(), "pb", "unexpected_failure")
			r.Flush(context.Background())
			if r.FlushStatus() != "drained" || r.SDKHTTPFailures() != 1 {
				t.Fatalf("fast failure was hidden: state=%s attempts_failed=%d", r.FlushStatus(), r.SDKHTTPFailures())
			}
		})
	}
}

type pausedMetricTransport struct {
	sentry.Transport
	once             sync.Once
	entered, release chan struct{}
}

func (t *pausedMetricTransport) SendEvent(event *sentry.Event) {
	if len(event.Metrics) != 0 {
		t.once.Do(func() { close(t.entered); <-t.release })
	}
	t.Transport.SendEvent(event)
}

func TestProductionBatchFlushWaitsForAlreadyPolledMetrics(t *testing.T) {
	var received int
	seen := map[string]int{}
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		mu.Lock()
		for _, line := range strings.Split(string(body), "\n") {
			var payload struct {
				Items []struct {
					Name string `json:"name"`
				} `json:"items"`
			}
			if json.Unmarshal([]byte(line), &payload) == nil {
				received += len(payload.Items)
				for _, item := range payload.Items {
					seen[item.Name]++
				}
			}
		}
		mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()
	httpTransport := sentry.NewHTTPTransport()
	httpTransport.BufferSize, httpTransport.Timeout = queueCapacity, flushLimit
	transport := &pausedMetricTransport{Transport: httpTransport, entered: make(chan struct{}), release: make(chan struct{})}
	defer func() {
		select {
		case <-transport.release:
		default:
			close(transport.release)
		}
	}()
	r := newSignalReporter(strings.Replace(server.URL, "http://", "http://public@", 1)+"/1", "test", transport, false, true, 0)
	defer r.Flush(context.Background())
	// Seed the already-polled SDK batch independently of the new producer budget.
	meter := sentry.NewMeter(r.context(context.Background()))
	for i := 0; i < 100; i++ {
		meter.Count("paperboat.operation.count", 1)
	}
	select {
	case <-transport.entered:
	case <-time.After(time.Second):
		t.Fatal("SDK did not poll its 100-item production metric batch")
	}
	// The blocked worker leaves its 100-item queue available for one real window:
	// 64 runtime snapshots, four diagnostic health gauges, 32 detail items.
	samples := make([]MetricSample, 0, 68)
	descriptors := make([]MetricDescriptor, 0, 68)
	for i := 0; i < snapshotLimit; i++ {
		name := fmt.Sprintf("paperboat_test_%d_snapshot", i)
		samples = append(samples, MetricSample{Name: name, Value: 1})
		descriptors = append(descriptors, MetricDescriptor{Name: name})
	}
	for _, name := range []string{DiagnosticRecordsDroppedMetric, DiagnosticRecordsFailedMetric, DiagnosticQueueDroppedMetric, DiagnosticPersistenceAvailableMetric} {
		samples = append(samples, MetricSample{Name: name, Value: 1})
		descriptors = append(descriptors, MetricDescriptor{Name: name})
	}
	first := true
	r.RegisterMetrics(func() []MetricSample {
		if first {
			first = false
			return samples
		}
		return nil
	}, descriptors)
	r.sampleMetrics()
	for i := 0; i < 16; i++ {
		r.Observe(context.Background(), "pb", "connect", "success", time.Millisecond)
	}
	r.Observe(context.Background(), "pb", "connect", "success", time.Millisecond)
	if r.Dropped()[1] != 2 {
		t.Fatalf("weighted detail rejection=%d", r.Dropped()[1])
	}
	done := make(chan struct{})
	go func() { r.Flush(context.Background()); close(done) }()
	select {
	case <-done:
		t.Fatal("flush passed a metric batch already polled by SDK worker")
	case <-time.After(20 * time.Millisecond):
	}
	close(transport.release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("healthy released metric batch did not drain")
	}
	mu.Lock()
	defer mu.Unlock()
	for _, name := range []string{DiagnosticRecordsDroppedMetric, DiagnosticRecordsFailedMetric, DiagnosticQueueDroppedMetric, DiagnosticPersistenceAvailableMetric} {
		if seen[name] != 1 {
			t.Fatalf("health receipt %s=%d", name, seen[name])
		}
	}
	if seen["paperboat.operation.duration"] != 16 || seen["paperboat.operation.count"] != 116 {
		t.Fatalf("weighted item receipts=%v", seen)
	}
	if received != 200 || r.FlushStatus() != "drained" || r.SDKHTTPFailures() != 0 {
		t.Fatalf("metrics before/after in-flight batch lost: received=%d state=%s", received, r.FlushStatus())
	}
}

func TestPausedFaultAndDelayedSpanCannotPublishAfterShutdown(t *testing.T) {
	transport := &sentry.MockTransport{}
	r := newSignalReporter("https://public@example.invalid/1", "test", transport, true, true, 1)
	ctx, finish := r.Start(context.Background(), "pb", "connect")
	entered, release, returned := make(chan struct{}), make(chan struct{}), make(chan struct{})
	restore := InstallFaultObserver(func(context.Context, Fault) { close(entered); <-release })
	defer restore()
	go func() {
		defer close(returned)
		r.CaptureFailure(ctx, "pb", "connect", "peer_connect", "transport_failed", syscall.ECONNREFUSED)
	}()
	<-entered
	r.Flush(context.Background())
	before, _ := json.Marshal(transport.Events())
	close(release)
	<-returned
	finish("failed")
	r.Observe(context.Background(), "pb", "connect", "failed", -1)
	r.Capture(context.Background(), "pb", "unexpected_failure")
	after, _ := json.Marshal(transport.Events())
	if string(before) != string(after) || r.SDKSubmissionsDropped() < 4 {
		t.Fatal("late publisher submitted after shutdown or was not accounted")
	}
}

func TestSamplerDeadlineSealsLateSnapshotAndCleansWorker(t *testing.T) {
	transport := &sentry.MockTransport{}
	r := newSignalReporter("https://public@example.invalid/1", "test", transport, false, true, 0)
	entered, release := make(chan struct{}), make(chan struct{})
	r.RegisterMetrics(func() []MetricSample {
		close(entered)
		<-release
		return []MetricSample{{Name: "paperboat_runtime_restart_total_snapshot", Value: 1}}
	}, []MetricDescriptor{{Name: "paperboat_runtime_restart_total_snapshot"}})
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	returned := make(chan struct{})
	started := time.Now()
	go func() { r.Flush(ctx); close(returned) }()
	<-entered
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("sampler blocked shutdown beyond deadline")
	}
	if time.Since(started) > time.Second || r.FlushStatus() != "timed_out" {
		t.Fatal("sampler timeout not exposed")
	}
	close(release)
	select {
	case <-r.samplerDone:
	case <-time.After(time.Second):
		t.Fatal("released sampler did not exit")
	}
	select {
	case <-r.flushDone:
	case <-time.After(time.Second):
		t.Fatal("shutdown cleanup did not finish")
	}
	if r.SnapshotDropped() != 1 || r.SDKSubmissionsDropped() == 0 {
		t.Fatal("late snapshot not fenced/accounted")
	}
	for _, event := range transport.Events() {
		if len(event.Metrics) != 0 {
			t.Fatal("late sampler submitted metrics to closed SDK")
		}
	}
}

func TestProductionTransportOutageHasBoundedShutdownStatus(t *testing.T) {
	entered, release := make(chan struct{}, 1), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release
		w.WriteHeader(503)
	}))
	defer server.Close()
	defer close(release)
	transport := sentry.NewHTTPTransport()
	transport.BufferSize = queueCapacity
	transport.Timeout = 100 * time.Millisecond
	r := newSignalReporter(strings.Replace(server.URL, "http://", "http://public@", 1)+"/1", "test", transport, true, false, 0)
	r.CaptureFailure(context.Background(), "pb", "connect", "peer_connect", "transport_failed", syscall.ECONNREFUSED)
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("production HTTP transport did not attempt send")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	started := time.Now()
	r.Flush(ctx)
	if time.Since(started) > time.Second || r.FlushStatus() != "timed_out" {
		t.Fatal("outage exceeded deadline or was presented as drained")
	}
	select {
	case <-r.flushDone:
	case <-time.After(time.Second):
		t.Fatal("SDK cleanup did not finish")
	}
}
