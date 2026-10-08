package observability

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-tunnel/internal/edgeerrors"
	"github.com/pinksaucepasta/paperboat-tunnel/internal/node"
	edgetelemetry "github.com/pinksaucepasta/paperboat-tunnel/internal/telemetry"
	"github.com/pinksaucepasta/paperboat-tunnel/internal/usage"
)

func TestEventAndErrorCannotLeakApplicationContent(t *testing.T) {
	event := Event{At: time.Unix(100, 0), Kind: Admission, Result: Rejected, RejectionCode: "credential_replayed", ConnectorGeneration: 3}
	encoded, err := event.JSON()
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"authorization", "cookie", "token", "body", "query", "public_host", "target"} {
		if strings.Contains(strings.ToLower(string(encoded)), forbidden) {
			t.Fatalf("event contains %q: %s", forbidden, encoded)
		}
	}
	secret := errors.New("Authorization: Bearer secret-token body=private")
	safe := Error(edgeerrors.Wrap(edgeerrors.CodeCredentialInvalid, "credential failed", "request a fresh admission", secret))
	if strings.Contains(safe.Code+safe.Recovery, "secret") || safe.Code != "credential_invalid" {
		t.Fatalf("safe error = %+v", safe)
	}
	if got := Error(secret); got.Code != "internal_error" || strings.Contains(got.Recovery, secret.Error()) {
		t.Fatalf("unknown error = %+v", got)
	}
}

func TestPrivateHandlerReportsBoundedDiagnosticsAndMetrics(t *testing.T) {
	state := node.New("edge_test")
	if !state.MarkReady() {
		t.Fatal("mark ready")
	}
	manager, _ := node.NewManager(state, 8)
	queue, _ := usage.NewQueue(4, 4096)
	now := time.Unix(200, 0).UTC()
	if err := queue.Enqueue(usage.Report{OperationID: "op", Interval: [2]time.Time{now.Add(-10 * time.Second), now}, Payload: []byte("signed-private-payload")}); err != nil {
		t.Fatal(err)
	}
	controlErr := errors.New("Authorization: Bearer secret")
	sessionRoutes := 2
	handler, err := NewHandler(Sources{Node: state.Snapshot, Manager: manager.Snapshot, Sessions: func() int { return 1 }, SessionRoutes: func() int { return sessionRoutes }, ActiveStreams: func() uint32 { return 3 }, RouteCount: func() int { return 2 }, Usage: queue.Stats, ControlErr: func() error { return controlErr }, RouteErr: func() error { return nil }, UsageErr: func() error { return nil }, CarrierRunning: func() bool { return true }, Traffic: usage.NewCounters().Snapshot, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	ready := httptest.NewRecorder()
	handler.ServeHTTP(ready, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if ready.Code != http.StatusServiceUnavailable || strings.Contains(ready.Body.String(), "secret") || strings.Contains(ready.Body.String(), "signed-private-payload") {
		t.Fatalf("ready response = %d %s", ready.Code, ready.Body.String())
	}
	if !strings.Contains(ready.Body.String(), `"control_unavailable"`) || !strings.Contains(ready.Body.String(), `"usage_pending_reports":1`) {
		t.Fatalf("diagnostics = %s", ready.Body.String())
	}
	metrics := httptest.NewRecorder()
	handler.ServeHTTP(metrics, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	for _, expected := range []string{"paperboat_tunnel_ready 0", "paperboat_tunnel_attached_routes 2", "paperboat_tunnel_usage_pending_reports 1", `dependency="control"} 0`} {
		if !strings.Contains(metrics.Body.String(), expected) {
			t.Fatalf("metrics missing %q: %s", expected, metrics.Body.String())
		}
	}
	controlErr = nil
	// An offline connector leaves a desired route without an active Carrier route;
	// this is normal availability state, not route ownership drift.
	sessionRoutes = 0
	offline := httptest.NewRecorder()
	handler.ServeHTTP(offline, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if offline.Code != http.StatusOK || strings.Contains(offline.Body.String(), `"route_drift":true`) {
		t.Fatalf("offline connector diagnostics = %d %s", offline.Code, offline.Body.String())
	}
	// More active session routes than authoritative attachments cannot be a
	// valid subset and must keep readiness closed.
	sessionRoutes = 3
	drift := httptest.NewRecorder()
	handler.ServeHTTP(drift, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if drift.Code != http.StatusServiceUnavailable || !strings.Contains(drift.Body.String(), `"route_drift":true`) || !strings.Contains(drift.Body.String(), `"route_drift"`) {
		t.Fatalf("route drift diagnostics = %d %s", drift.Code, drift.Body.String())
	}
}

func TestDiagnosticsDistinguishDependencies(t *testing.T) {
	healthy := Diagnostics{Control: Healthy, Store: Healthy, Carrier: Healthy, Usage: Healthy}
	if !healthy.Ready() {
		t.Fatal("healthy node is not ready")
	}
	healthy.Control = Degraded
	if healthy.Ready() {
		t.Fatal("control degradation remained ready")
	}
	healthy.Control, healthy.Usage = Healthy, Degraded
	if !healthy.Ready() {
		t.Fatal("bounded usage delivery degradation should remain ready")
	}
	healthy.Usage = Unavailable
	if healthy.Ready() {
		t.Fatal("undurable usage remained ready")
	}
}

func TestPrivateHandlerProjectsTypedHealthEventsMetricsAndDrops(t *testing.T) {
	state := node.New("edge_test")
	if !state.MarkReady() {
		t.Fatal("mark ready")
	}
	manager, _ := node.NewManager(state, 8)
	queue, _ := usage.NewQueue(4, 4096)
	now := time.Unix(300, 0).UTC()
	health, err := edgetelemetry.NewHealthTracker(func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	if err := health.Update(edgetelemetry.HealthUpdate{Dimension: edgetelemetry.DimensionRoute, Status: edgetelemetry.StatusReady, Code: "ready", Summary: "Route is ready.", RepairAction: "No action is required.", Retry: edgetelemetry.RetryNone}); err != nil {
		t.Fatal(err)
	}
	events, err := edgetelemetry.NewEventLog(4)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := events.Record(edgetelemetry.EventInput{At: now, Severity: edgetelemetry.SeverityInfo, Component: edgetelemetry.DimensionRoute, Name: "activated", Code: "activated", Outcome: edgetelemetry.OutcomeStateChange, Message: "Route generation activated.", CorrelationID: "corr_route_1", Retry: edgetelemetry.RetryNone}); err != nil {
		t.Fatal(err)
	}
	typedMetrics := edgetelemetry.NewMetrics()
	if err := typedMetrics.AddCounter(edgetelemetry.MetricRouteRequests, edgetelemetry.MetricLabels{"route_kind": "tunnel_https_wss", "outcome": "success"}, 2); err != nil {
		t.Fatal(err)
	}
	handler, err := NewHandler(Sources{Node: state.Snapshot, Manager: manager.Snapshot, Sessions: func() int { return 0 }, SessionRoutes: func() int { return 0 }, ActiveStreams: func() uint32 { return 0 }, RouteCount: func() int { return 0 }, Usage: queue.Stats, ControlErr: func() error { return nil }, RouteErr: func() error { return nil }, UsageErr: func() error { return nil }, CarrierRunning: func() bool { return true }, Traffic: usage.NewCounters().Snapshot, Health: health.Snapshot, Lifecycle: events.Snapshot, TypedMetrics: typedMetrics.Snapshot, TelemetryDrops: func() uint64 { return 3 }, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	diagnostics := httptest.NewRecorder()
	handler.ServeHTTP(diagnostics, httptest.NewRequest(http.MethodGet, "/diagnostics", nil))
	for _, expected := range []string{`"schema":"paperboat.health/v1"`, `"name":"activated"`, `"telemetry_drops":3`} {
		if !strings.Contains(diagnostics.Body.String(), expected) {
			t.Fatalf("diagnostics missing %q: %s", expected, diagnostics.Body.String())
		}
	}
	metrics := httptest.NewRecorder()
	handler.ServeHTTP(metrics, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	for _, expected := range []string{`paperboat_edge_route_requests_total{route_kind="tunnel_https_wss",outcome="success"} 2`, `paperboat_edge_telemetry_dropped_total 3`} {
		if !strings.Contains(metrics.Body.String(), expected) {
			t.Fatalf("metrics missing %q: %s", expected, metrics.Body.String())
		}
	}
}

func TestMetricDescriptorsFollowTypedTelemetryCatalog(t *testing.T) {
	descriptors := MetricDescriptors()
	byName := make(map[string]MetricDescriptor, len(descriptors))
	for _, descriptor := range descriptors {
		byName[descriptor.Name] = descriptor
	}
	for _, typed := range edgetelemetry.MetricDescriptors() {
		descriptor, ok := byName[typed.Name]
		if !ok || descriptor.Kind != string(typed.Kind) {
			t.Fatalf("typed metric %q missing from endpoint descriptors", typed.Name)
		}
		if typed.Histogram != nil && len(descriptor.Buckets) != len(typed.Histogram.Buckets) {
			t.Fatalf("histogram %q buckets=%v want=%v", typed.Name, descriptor.Buckets, typed.Histogram.Buckets)
		}
	}
	if _, ok := byName["paperboat_tunnel_events_total"]; ok {
		t.Fatal("unused event counter remains documented")
	}
}

func TestErrorProjectionUsesOwnedCodesAndRecovery(t *testing.T) {
	for _, err := range []error{edgeerrors.New(edgeerrors.Code("PRIVATE_CODE"), "PRIVATE_MESSAGE", "PRIVATE_RECOVERY"), edgeerrors.New(edgeerrors.CodeCredentialInvalid, "PRIVATE_MESSAGE", "PRIVATE_RECOVERY"), errors.Join(errors.New("PRIVATE_CAUSE"), edgeerrors.New(edgeerrors.CodeRevoked, "PRIVATE_MESSAGE", "PRIVATE_RECOVERY"))} {
		encoded, _ := json.Marshal(Error(err))
		if strings.Contains(string(encoded), "PRIVATE_") {
			t.Fatal("private typed error fields exported")
		}
	}
	if actual := Error(edgeerrors.New(edgeerrors.CodeCredentialInvalid, "private", "private")); actual.Code != "credential_invalid" || actual.Recovery != "request a fresh admission" {
		t.Fatalf("owned recovery lost: %+v", actual)
	}
	var nilError *edgeerrors.Error
	if actual := Error(nilError); actual.Code != "internal_error" {
		t.Fatal("typed nil accepted")
	}
}
