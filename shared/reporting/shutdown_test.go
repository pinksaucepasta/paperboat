package reporting

import (
	"context"
	"encoding/json"
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

// Match New's production constructor: explicit eight-envelope HTTPTransport,
// the pinned legacy batch path, and only a response/network observer underneath.
func productionFixture(t *testing.T, serverURL string, transport sentry.Transport, logs, metrics, traces bool) *Reporter {
	t.Helper()
	r := &Reporter{enabled: true, component: serviceComponentName, logs: logs, metrics: metrics, traces: traces, localFault: func(Fault) {}}
	config := options(serviceComponentName, "release", "http://public@"+strings.TrimPrefix(serverURL, "http://")+"/1", transport)
	config.HTTPTransport = sdkHTTPObserver{next: &http.Transport{Proxy: http.ProxyFromEnvironment}, failures: &r.httpFailures}
	config.EnableTracing, config.TracesSampleRate = traces, 1
	client, err := sentry.NewClient(config)
	if err != nil {
		t.Fatal(err)
	}
	r.client, r.hub = client, sentry.NewHub(client, sentry.NewScope())
	if !client.Options().DisableTelemetryBuffer || client.Options().Transport != transport {
		t.Fatal("fixture does not match production SDK path")
	}
	t.Cleanup(r.Close)
	return r
}
func productionHTTPTransport() *sentry.HTTPTransport {
	t := sentry.NewHTTPTransport()
	t.BufferSize, t.Timeout = maxEvents, flushTimeout
	return t
}

func TestProductionShutdownHealthySignalsAndHTTPFailures(t *testing.T) {
	for _, scenario := range []string{"healthy", "503", "network"} {
		t.Run(scenario, func(t *testing.T) {
			var mu sync.Mutex
			var bodies []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				body, _ := io.ReadAll(req.Body)
				mu.Lock()
				bodies = append(bodies, string(body))
				mu.Unlock()
				if scenario == "503" {
					w.WriteHeader(503)
				} else {
					w.WriteHeader(202)
				}
			}))
			defer server.Close()
			if scenario == "network" {
				server.Close()
			}
			healthy := scenario == "healthy"
			r := productionFixture(t, server.URL, productionHTTPTransport(), healthy, healthy, healthy)
			r.CaptureFailure(context.Background(), fixtureFailure, syscall.ECONNREFUSED)
			if healthy {
				_, ref, finish := r.ControlTrace(context.Background(), fixtureOperation)
				finish("success", "ok")
				if !validReference(ref) {
					t.Fatal("canonical reference absent")
				}
				r.ExportDrops(context.Background())
				r.MetricSnapshots(context.Background(), []Snapshot{{Name: fixtureSnapshot, Kind: "gauge", Value: 1}})
			}
			r.Close()
			if r.FlushStatus() != "drained" {
				t.Fatalf("shutdown=%s", r.FlushStatus())
			}
			if healthy {
				mu.Lock()
				joined := strings.Join(bodies, "\n")
				mu.Unlock()
				for _, kind := range []string{`"type":"event"`, `"type":"log"`, `"type":"transaction"`, `"type":"trace_metric"`, fixtureSnapshot} {
					if !strings.Contains(joined, kind) {
						t.Fatalf("healthy HTTP202 missing %s", kind)
					}
				}
				if r.SDKHTTPFailures() != 0 {
					t.Fatal("successful responses counted as failures")
				}
			} else if r.SDKHTTPFailures() != 1 {
				t.Fatalf("failed HTTP attempts=%d", r.SDKHTTPFailures())
			}
		})
	}
}

func TestPreSubmitFaultAndDelayedSpanStayLocalAfterShutdown(t *testing.T) {
	transport := &sentry.MockTransport{}
	r := productionFixture(t, "http://example.invalid", transport, true, true, true)
	_, _, finish := r.ControlTrace(context.Background(), fixtureOperation)
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	r.localFault = func(Fault) { close(entered); <-release }
	go func() {
		defer close(done)
		r.CaptureFailure(context.Background(), fixtureFailure, syscall.ECONNREFUSED)
	}()
	<-entered
	r.Close()
	before, _ := json.Marshal(transport.Events())
	close(release)
	<-done
	finish("failed", "internal")
	r.Observe(context.Background(), fixtureOperation, "success", "ok", "", 0)
	r.ExportDrops(context.Background())
	r.MetricSnapshots(context.Background(), []Snapshot{{Name: fixtureSnapshot, Kind: "gauge", Value: 1}})
	after, _ := json.Marshal(transport.Events())
	if string(before) != string(after) || r.SDKSubmissionsDropped() != 5 {
		t.Fatalf("late publication: dropped=%d", r.SDKSubmissionsDropped())
	}
}

type pausedExportTransport struct {
	sentry.Transport
	metricsOnly      bool
	once             sync.Once
	entered, release chan struct{}
}

func (t *pausedExportTransport) SendEvent(event *sentry.Event) {
	if !t.metricsOnly || len(event.Metrics) > 0 {
		t.once.Do(func() { close(t.entered); <-t.release })
	}
	t.Transport.SendEvent(event)
}

func TestProductionInFlightMetricBatchAndReservedHealthBudget(t *testing.T) {
	var mu sync.Mutex
	var received int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		data, _ := io.ReadAll(req.Body)
		mu.Lock()
		received += metricItemCount(t, data)
		mu.Unlock()
		w.WriteHeader(202)
	}))
	defer server.Close()
	transport := &pausedExportTransport{Transport: productionHTTPTransport(), metricsOnly: true, entered: make(chan struct{}), release: make(chan struct{})}
	defer func() {
		select {
		case <-transport.release:
		default:
			close(transport.release)
		}
	}()
	r := productionFixture(t, server.URL, transport, false, true, false)
	for i := 0; i < maxDetailMetrics/2+1; i++ {
		r.Observe(context.Background(), fixtureOperation, "success", "ok", "", 0)
	}
	snapshots := make([]Snapshot, maxSnapshots)
	for i := range snapshots {
		snapshots[i] = Snapshot{Name: fixtureSnapshot, Kind: "gauge", Value: float64(i)}
	}
	r.ExportDrops(context.Background())
	r.MetricSnapshots(context.Background(), snapshots)
	select {
	case <-transport.entered:
	case <-time.After(time.Second):
		t.Fatal("SDK did not poll 100-item reserved-budget batch")
	}
	// The first batch is already outside its queue. These final health items
	// must also be drained, while Close waits for that in-flight sendBatch.
	r.ExportDrops(context.Background())
	done := make(chan struct{})
	go func() { r.Close(); close(done) }()
	select {
	case <-done:
		t.Fatal("close passed in-flight SDK metric batch")
	case <-time.After(20 * time.Millisecond):
	}
	close(transport.release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("healthy released batch did not drain")
	}
	mu.Lock()
	defer mu.Unlock()
	if received != 108 || r.metricsSent.Load() != uint32(maxDetailMetrics) || r.droppedMetrics.Load() != 2 || r.FlushStatus() != "drained" {
		t.Fatalf("reserved health items lost: received=%d detail=%d drops=%d state=%s", received, r.metricsSent.Load(), r.droppedMetrics.Load(), r.FlushStatus())
	}
}

func TestShutdownDeadlineBoundsBlockedSDKJoinAndFencesNewProducers(t *testing.T) {
	transport := &pausedExportTransport{Transport: &sentry.MockTransport{}, entered: make(chan struct{}), release: make(chan struct{})}
	r := productionFixture(t, "http://example.invalid", transport, false, false, false)
	submitted := make(chan struct{})
	go func() {
		defer close(submitted)
		r.CaptureFailure(context.Background(), fixtureFailure, syscall.ECONNREFUSED)
	}()
	<-transport.entered
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	started := time.Now()
	r.CloseContext(ctx)
	if time.Since(started) > time.Second || r.FlushStatus() != "timed_out" {
		t.Fatal("blocked SDK join exceeded deadline")
	}
	r.CaptureFailure(context.Background(), fixtureFailure, syscall.ECONNREFUSED)
	if r.SDKSubmissionsDropped() != 1 {
		t.Fatal("closed gate admitted another producer")
	}
	close(transport.release)
	<-submitted
	select {
	case <-r.closeDone:
	case <-time.After(time.Second):
		t.Fatal("released SDK worker did not clean up")
	}
}

func TestProductionOutageBoundsWholeShutdown(t *testing.T) {
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
	transport := productionHTTPTransport()
	transport.Timeout = 100 * time.Millisecond
	r := productionFixture(t, server.URL, transport, false, false, false)
	r.CaptureFailure(context.Background(), fixtureFailure, syscall.ECONNREFUSED)
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("production SDK did not attempt export")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	started := time.Now()
	r.CloseContext(ctx)
	if time.Since(started) > time.Second || r.FlushStatus() != "timed_out" {
		t.Fatal("outage presented as drained or exceeded deadline")
	}
	select {
	case <-r.closeDone:
	case <-time.After(time.Second):
		t.Fatal("SDK cleanup did not finish")
	}
}

const (
	serviceComponentName = "paperboat-relay"
	fixtureOperation     = "relay_session"
	fixtureSnapshot      = "paperboat_relay_sessions"
	fixtureFailure       = "service_start"
)

// Count payload metric items, excluding the envelope's SDK name metadata.
func metricItemCount(t *testing.T, data []byte) int {
	t.Helper()
	count := 0
	for _, line := range strings.Split(string(data), "\n") {
		var item struct {
			Items []json.RawMessage `json:"items"`
		}
		if len(line) > 0 && json.Unmarshal([]byte(line), &item) != nil {
			t.Error("invalid SDK envelope JSON")
		}
		count += len(item.Items)
	}
	return count
}
