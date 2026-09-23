package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-tunnel/internal/config"
	"github.com/pinksaucepasta/paperboat-tunnel/internal/datacarrier"
	"github.com/pinksaucepasta/paperboat-tunnel/internal/edgehttp"
	edgetelemetry "github.com/pinksaucepasta/paperboat-tunnel/internal/telemetry"
)

func TestCarrierEndpointFromDeploymentIsSeparateAndComplete(t *testing.T) {
	deployment := config.Deployment{ConnectorAdvertiseHost: "edge.example.test", CarrierTCPListenAddress: "0.0.0.0:27443", CarrierQUICListenAddress: "0.0.0.0:27444"}
	endpoint, err := carrierEndpointFromDeployment(deployment)
	if err != nil {
		t.Fatal(err)
	}
	if endpoint == nil || endpoint.Host != "edge.example.test" || endpoint.TCPPort != 27443 || endpoint.QUICPort != 27444 {
		t.Fatalf("carrier endpoint = %+v", endpoint)
	}
	if _, err := carrierEndpointFromDeployment(config.Deployment{ConnectorAdvertiseHost: "edge.example.test", CarrierTCPListenAddress: "0.0.0.0:27443"}); err == nil {
		t.Fatal("incomplete carrier endpoint accepted")
	}
	if endpoint, err := carrierEndpointFromDeployment(config.Deployment{}); err == nil || endpoint != nil {
		t.Fatalf("empty carrier endpoint = %+v, %v", endpoint, err)
	}
}

func TestLiveCarrierHTTPCompositionPreservesStreamingAndSharesTelemetry(t *testing.T) {
	metrics := edgetelemetry.NewMetrics()
	events, err := edgetelemetry.NewEventLogWithQueue(64, 64)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = events.Close() })
	carrierTelemetry, err := datacarrier.NewCarrierTelemetry(datacarrier.CarrierTelemetryConfig{
		Metrics: metrics,
		Events:  events,
		Info: datacarrier.StreamInfo{
			IDs:           edgetelemetry.SafeIDs{EdgeNodeID: "edge_1"},
			CorrelationID: "corr_edge_carrier",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	requestTelemetry, err := edgehttp.NewRequestTelemetry(edgehttp.RequestTelemetryConfig{Metrics: metrics, Events: events, Info: edgehttp.RequestInfo{IDs: edgetelemetry.SafeIDs{EdgeNodeID: "edge_1"}, RouteKind: "tunnel_https_wss"}})
	if err != nil {
		t.Fatal(err)
	}
	gatewayTelemetry, err := edgehttp.NewRequestTelemetry(edgehttp.RequestTelemetryConfig{Metrics: metrics, Events: events, Info: edgehttp.RequestInfo{IDs: edgetelemetry.SafeIDs{EdgeNodeID: "edge_1"}}})
	if err != nil {
		t.Fatal(err)
	}
	gateway := gatewayTelemetry.Handler(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusNoContent)
	}))
	gatewayRecorder := httptest.NewRecorder()
	gateway.ServeHTTP(gatewayRecorder, httptest.NewRequest(http.MethodGet, "https://gateway.example.test/health", nil))
	if gatewayRecorder.Code != http.StatusNoContent {
		t.Fatalf("gateway status = %d", gatewayRecorder.Code)
	}
	transport := requestTelemetry.RoundTripper(&carrierTelemetryRoundTripper{
		Next: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("streamed response")), Request: request}, nil
		}),
		Telemetry: carrierTelemetry,
		RouteKind: "tunnel_https_wss",
		NodeID:    "edge_1",
	})
	request := httptest.NewRequest(http.MethodGet, "https://tunnel.example.test/stream", nil)
	response, err := transport.RoundTrip(request)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(payload) != "streamed response" {
		t.Fatalf("response body = %q", payload)
	}
	if err := response.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if got := requestMetricValue(metrics.Snapshot(), edgetelemetry.MetricActiveStreams); got != 0 {
		t.Fatalf("active carrier streams = %d", got)
	}
	if got := requestMetricValue(metrics.Snapshot(), edgetelemetry.MetricTrafficBytes, "ingress"); got != uint64(len("streamed response")) {
		t.Fatalf("carrier response bytes = %d", got)
	}
	if got := requestMetricValue(metrics.Snapshot(), edgetelemetry.MetricRouteRequests, "tunnel_https_wss", "success"); got != 1 {
		t.Fatalf("tunnel route request successes = %d", got)
	}
	if got := requestMetricValue(metrics.Snapshot(), edgetelemetry.MetricEdgeRequests, "https", "success"); got != 1 {
		t.Fatalf("gateway edge request successes = %d", got)
	}
	var opened, closed int
	for _, event := range events.Snapshot() {
		switch event.Name {
		case "carrier_stream_opened":
			opened++
		case "carrier_stream_closed":
			closed++
		}
	}
	if opened != 1 || closed != 1 {
		t.Fatalf("carrier stream lifecycle opened=%d closed=%d", opened, closed)
	}
}

func TestTelemetryLifecycleReportsStartupReadinessAndShutdown(t *testing.T) {
	health, err := edgetelemetry.NewHealthTracker(time.Now)
	if err != nil {
		t.Fatal(err)
	}
	metrics := edgetelemetry.NewMetrics()
	events, err := edgetelemetry.NewEventLogWithQueue(16, 16)
	if err != nil {
		t.Fatal(err)
	}
	lifecycle := newTelemetryLifecycle(health, metrics, events)
	if err := lifecycle.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if state := health.Snapshot().Dimensions.Service; state.Status != edgetelemetry.StatusDegraded || state.Code != "service_starting" {
		t.Fatalf("starting health = %+v", state)
	}
	if err := lifecycle.MarkReady(context.Background()); err != nil {
		t.Fatal(err)
	}
	if state := health.Snapshot().Dimensions.Service; state.Status != edgetelemetry.StatusReady || state.Code != "service_ready" {
		t.Fatalf("ready health = %+v", state)
	}
	if err := lifecycle.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if state := health.Snapshot().Dimensions.Service; state.Status != edgetelemetry.StatusDown || state.Code != "service_shutdown" {
		t.Fatalf("shutdown health = %+v", state)
	}
	var names []string
	for _, event := range events.Snapshot() {
		names = append(names, event.Name)
	}
	if len(names) != 3 || names[0] != "edge_service_starting" || names[1] != "edge_service_ready" || names[2] != "edge_service_shutdown" {
		t.Fatalf("lifecycle events = %v", names)
	}
	if _, accepted, err := events.TryRecord(edgetelemetry.EventInput{At: time.Now(), Severity: edgetelemetry.SeverityInfo, Component: edgetelemetry.DimensionService, Name: "after_close", Code: "after_close", Outcome: edgetelemetry.OutcomeStateChange, Message: "ignored", CorrelationID: "corr_after_close", Retry: edgetelemetry.RetryNone}); err != nil || accepted {
		t.Fatalf("event after close accepted=%v err=%v", accepted, err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

func requestMetricValue(samples []edgetelemetry.MetricSample, name string, labels ...string) uint64 {
	for _, sample := range samples {
		if sample.Name != name || len(sample.Labels) != len(labels) {
			continue
		}
		match := true
		for index, value := range labels {
			if sample.Labels[index].Value != value {
				match = false
				break
			}
		}
		if match {
			return sample.Value
		}
	}
	return 0
}
