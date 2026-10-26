package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/buildinfo"
	"github.com/pinksaucepasta/paperboat/server/internal/config"
	serverdiagnostics "github.com/pinksaucepasta/paperboat/server/internal/diagnostics"
	"github.com/pinksaucepasta/paperboat/server/internal/telemetry"
)

type readinessFunc func(context.Context) error

func (f readinessFunc) Ready(ctx context.Context) error {
	return f(ctx)
}

func TestTunnelCreateAuthBypassesClientAuthForMachineRequest(t *testing.T) {
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	})
	request := httptest.NewRequest(http.MethodPost, "/v1/tunnels", strings.NewReader(`{"name":"demo"}`))
	request.Header.Set("Authorization", "Bearer machine-control")
	request.Header.Set("X-Paperboat-Machine-Identity", "machine-control")
	request.Header.Set("X-Paperboat-Machine-Proof", "proof")
	recorder := httptest.NewRecorder()
	tunnelCreateAuth(nil, nil, next).ServeHTTP(recorder, request)
	if !called || recorder.Code != http.StatusNoContent {
		t.Fatalf("machine create wrapper status=%d called=%v", recorder.Code, called)
	}
}

func TestHealthDoesNotRequireReadiness(t *testing.T) {
	router := NewRouter(Options{
		Config: config.Default(),
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		ReadinessChecker: readinessFunc(func(context.Context) error {
			return errors.New("not ready")
		}),
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Request-Id") == "" {
		t.Fatal("missing request id")
	}
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("missing secure headers")
	}
}

func TestTelemetryRouteFamilyIsFinite(t *testing.T) {
	tests := map[string]string{
		"/healthz":                     "health",
		"/v1/edge/routes":              "edge_control",
		"/install":                     "release",
		"/tuf/root.json":               "release",
		"/v1/previews/preview_1":       "public_api",
		"/customer.example/token/path": "other",
	}
	for path, want := range tests {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		if got := telemetryRouteFamily(req); got != want {
			t.Fatalf("path %q: got %q want %q", path, got, want)
		}
	}
}

func TestRouterTelemetryUsesTrustedRequestCorrelation(t *testing.T) {
	events, err := telemetry.NewEventLog(4)
	if err != nil {
		t.Fatal(err)
	}
	defer events.Close()
	metrics := telemetry.NewMetrics()
	observer := &telemetry.HTTPObserver{
		Metrics: metrics,
		Events:  events,
		Identity: func(ctx context.Context) (string, string) {
			return serverdiagnostics.RequestID(ctx), serverdiagnostics.CorrelationID(ctx)
		},
	}
	router := NewRouter(Options{Config: config.Default(), Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), TelemetryHTTP: observer})
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/v1/customer.example/secret-path", nil)
	request.Header.Set("Request-Id", "untrusted/path")
	router.ServeHTTP(recorder, request)
	correlationID := recorder.Header().Get("Correlation-Id")
	if correlationID == "" || !strings.Contains(recorder.Body.String(), `"correlation_id":"`+correlationID+`"`) {
		t.Fatalf("API error omitted its response correlation ID %q: %s", correlationID, recorder.Body.String())
	}
	recorded := events.Snapshot()
	if len(recorded) != 1 || recorded[0].RequestID == "" || recorded[0].CorrelationID == "" || recorded[0].RequestID == "untrusted/path" {
		t.Fatalf("events=%#v", recorded)
	}
	encoded, err := recorded[0].JSON()
	if err != nil {
		t.Fatal(err)
	}
	for _, unsafe := range []string{"customer.example", "secret-path", "untrusted/path"} {
		if strings.Contains(string(encoded), unsafe) {
			t.Fatalf("event leaked %q: %s", unsafe, encoded)
		}
	}
}

func TestRouterTelemetryRecordsTimeoutResponse(t *testing.T) {
	events, err := telemetry.NewEventLog(8)
	if err != nil {
		t.Fatal(err)
	}
	defer events.Close()
	metrics := telemetry.NewMetrics()
	observer := &telemetry.HTTPObserver{
		Metrics: metrics,
		Events:  events,
		Identity: func(ctx context.Context) (string, string) {
			return serverdiagnostics.RequestID(ctx), serverdiagnostics.CorrelationID(ctx)
		},
	}
	started := make(chan struct{})
	release := make(chan struct{})
	handlerDone := make(chan struct{})
	cfg := config.Default()
	cfg.HTTP.RequestTimeout = 20 * time.Millisecond
	router := NewRouter(Options{
		Config: cfg, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), TelemetryHTTP: observer,
		OverrideHandler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			close(started)
			<-release
			close(handlerDone)
		}),
	})
	response := httptest.NewRecorder()
	requestDone := make(chan struct{})
	go func() {
		router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/slow", nil))
		close(requestDone)
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("timed request handler did not start")
	}
	select {
	case <-requestDone:
	case <-time.After(time.Second):
		t.Fatal("request timeout did not return")
	}
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("timeout status=%d body=%s", response.Code, response.Body.String())
	}
	close(release)
	select {
	case <-handlerDone:
	case <-time.After(time.Second):
		t.Fatal("timed request handler did not exit")
	}
	var requestSample bool
	for _, sample := range metrics.Snapshot() {
		if sample.Name != telemetry.MetricHTTPRequests {
			continue
		}
		for _, label := range sample.Labels {
			if label.Name == "status_class" && label.Value == "5xx" {
				requestSample = true
			}
		}
	}
	if !requestSample {
		t.Fatalf("timeout response was not recorded as 5xx: %#v", metrics.Snapshot())
	}
	recorded := events.Snapshot()
	if len(recorded) != 1 || recorded[0].Code != "http_request_failed" || recorded[0].Outcome != telemetry.OutcomeFailed {
		t.Fatalf("timeout event=%#v", recorded)
	}
}

func TestDiagnosticUploadUnavailableDoesNotFallThroughToEdgeControl(t *testing.T) {
	edge := http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusUnauthorized)
		_, _ = writer.Write([]byte(`{"code":"edge_control"}`))
	})
	router := NewRouter(Options{Config: config.Default(), Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), EdgeControl: edge})
	for _, path := range []string{"/v1/diagnostic-upload-intents", "/v1/diagnostic-upload-intents/diag_test/complete"} {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`))
		router.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusServiceUnavailable || !strings.Contains(recorder.Body.String(), `"code":"diagnostic_upload_unavailable"`) || strings.Contains(recorder.Body.String(), "edge_control") {
			t.Fatalf("path=%s status=%d body=%s", path, recorder.Code, recorder.Body.String())
		}
	}
}

func TestRequestIDRejectsUnsafeClientValue(t *testing.T) {
	router := NewRouter(Options{Config: config.Default(), Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.Header.Set("Request-Id", "user supplied/path")
	router.ServeHTTP(rec, req)
	got := rec.Header().Get("Request-Id")
	if got == "" || got == "user supplied/path" {
		t.Fatalf("request id = %q", got)
	}
}

func TestMetricsAreLocalOnly(t *testing.T) {
	typed := telemetry.NewMetrics()
	if err := typed.IncCounter(telemetry.MetricHTTPRequests, telemetry.MetricLabels{"method": "get", "route_family": "health", "status_class": "2xx"}); err != nil {
		t.Fatal(err)
	}
	router := NewRouter(Options{Config: config.Default(), Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), TelemetryMetrics: typed})
	local := httptest.NewRecorder()
	localReq := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	localReq.RemoteAddr = "127.0.0.1:1234"
	router.ServeHTTP(local, localReq)
	if local.Code != http.StatusOK || !strings.Contains(local.Body.String(), "device_requested_total") {
		t.Fatalf("local status = %d, body = %s", local.Code, local.Body.String())
	}
	if !strings.Contains(local.Body.String(), telemetry.MetricHTTPRequests) {
		t.Fatalf("typed metrics missing: %s", local.Body.String())
	}
	remote := httptest.NewRecorder()
	remoteReq := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	remoteReq.RemoteAddr = "203.0.113.10:1234"
	router.ServeHTTP(remote, remoteReq)
	if remote.Code != http.StatusForbidden {
		t.Fatalf("remote status = %d, body = %s", remote.Code, remote.Body.String())
	}
}

func TestReadyReflectsDependencyState(t *testing.T) {
	router := NewRouter(Options{
		Config: config.Default(),
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		ReadinessChecker: readinessFunc(func(context.Context) error {
			return errors.New("db down")
		}),
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var payload ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Error.Code != "provider_unavailable" {
		t.Fatalf("error code = %q", payload.Error.Code)
	}
}

func TestCORSAllowsConfiguredOrigins(t *testing.T) {
	cfg := config.Default()
	cfg.HTTP.AllowedOrigins = []string{"https://dashboard.example"}
	router := NewRouter(Options{
		Config: cfg,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		ReadinessChecker: readinessFunc(func(context.Context) error {
			return nil
		}),
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodOptions, "/v1/projects", strings.NewReader(""))
	req.Header.Set("Origin", "https://dashboard.example")
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://dashboard.example" {
		t.Fatalf("allow origin = %q", got)
	}
}

func TestUnknownEndpointIsStructuredError(t *testing.T) {
	router := NewRouter(Options{
		Config: config.Default(),
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		ReadinessChecker: readinessFunc(func(context.Context) error {
			return nil
		}),
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/unknown-resource", nil)
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"error"`) {
		t.Fatalf("expected structured error, got %s", rec.Body.String())
	}
}

func TestTimeoutUsesStructuredError(t *testing.T) {
	for iteration := 0; iteration < 32; iteration++ {
		handler := requestID(timeout(time.Nanosecond, slog.New(slog.NewTextHandler(io.Discard, nil)), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			<-r.Context().Done()
		})))
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/slow", nil)
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
		var payload ErrorResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
			t.Fatal(err)
		}
		if payload.Error.Code != "provider_unavailable" {
			t.Fatalf("error code = %q", payload.Error.Code)
		}
		if payload.Error.RequestID == "" {
			t.Fatal("missing request id in timeout response")
		}
	}
}

func TestTimeoutIsolatesLateHandlerHeaders(t *testing.T) {
	releaseHandler := make(chan struct{})
	handlerDone := make(chan struct{})
	handler := requestID(timeout(time.Nanosecond, slog.New(slog.NewTextHandler(io.Discard, nil)), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(handlerDone)
		<-r.Context().Done()
		<-releaseHandler
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("X-Late-Header", "ignored")
		if _, err := w.Write([]byte("late")); !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("late write error = %v, want context deadline exceeded", err)
		}
	})))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/slow", nil)
	handler.ServeHTTP(rec, req)
	close(releaseHandler)
	select {
	case <-handlerDone:
	case <-time.After(time.Second):
		t.Fatal("handler did not finish after timeout")
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("content type = %q", got)
	}
	if got := rec.Header().Get("X-Late-Header"); got != "" {
		t.Fatalf("late header leaked into response: %q", got)
	}
}

func TestTimeoutRecoversHandlerPanic(t *testing.T) {
	handler := requestID(timeout(time.Second, slog.New(slog.NewTextHandler(io.Discard, nil)), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	})))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/panic", nil)
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var payload ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Error.Code != "internal_error" {
		t.Fatalf("error code = %q", payload.Error.Code)
	}
	if payload.Error.RequestID == "" {
		t.Fatal("missing request id in panic response")
	}
}

func TestTimeoutDoesNotBufferStreamingResponse(t *testing.T) {
	handlerDone := make(chan struct{})
	handler := requestID(timeout(10*time.Millisecond, slog.New(slog.NewTextHandler(io.Discard, nil)), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(handlerDone)
		w.WriteHeader(http.StatusOK)
		if _, err := w.Write([]byte("event: ready\n\n")); err != nil {
			t.Errorf("write stream event: %v", err)
		}
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
	})))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/previews/prv_1/events", nil)
	req.Header.Set("Accept", "text/event-stream")
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != "event: ready\n\n" {
		t.Fatalf("body = %q", got)
	}
	if !rec.Flushed {
		t.Fatal("streaming response was not flushed")
	}
	select {
	case <-handlerDone:
	case <-time.After(time.Second):
		t.Fatal("streaming handler did not complete")
	}
}

func TestTimeoutBypassesStreamingRequestDeadline(t *testing.T) {
	handler := requestID(timeout(time.Nanosecond, slog.New(slog.NewTextHandler(io.Discard, nil)), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
			t.Fatal("streaming request context was canceled by request timeout")
		default:
		}
		w.WriteHeader(http.StatusOK)
	})))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/previews/prv_1/events", nil)
	req.Header.Set("Accept", "text/event-stream")
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func TestTimeoutBypassesWebSocketControlLifetime(t *testing.T) {
	handler := requestID(timeout(time.Nanosecond, slog.New(slog.NewTextHandler(io.Discard, nil)), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
			t.Fatal("websocket request context was canceled by ordinary request timeout")
		default:
		}
		w.WriteHeader(http.StatusSwitchingProtocols)
	})))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/tunnels/tunnel_01/connectors/connector_01/control", nil)
	req.Header.Set("Connection", "keep-alive, Upgrade")
	req.Header.Set("Upgrade", "websocket")
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusSwitchingProtocols {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func TestRouterPreservesStreamingFlushSupport(t *testing.T) {
	cfg := config.Default()
	cfg.HTTP.RequestTimeout = time.Nanosecond
	router := NewRouter(Options{
		Config: cfg,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		ReadinessChecker: readinessFunc(func(context.Context) error {
			return nil
		}),
		OverrideHandler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			if _, err := w.Write([]byte("event: ready\n\n")); err != nil {
				t.Errorf("write stream event: %v", err)
			}
			flusher, ok := w.(http.Flusher)
			if !ok {
				t.Fatal("response writer does not expose http.Flusher")
			}
			flusher.Flush()
		}),
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/previews/prv_1/events", nil)
	req.Header.Set("Accept", "text/event-stream")
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != "event: ready\n\n" {
		t.Fatalf("body = %q", got)
	}
	if !rec.Flushed {
		t.Fatal("streaming response was not flushed through router stack")
	}
}

func TestServerVersionReportsTheBuildWithoutAuthentication(t *testing.T) {
	previous := [2]string{buildinfo.Version, buildinfo.Commit}
	buildinfo.Version, buildinfo.Commit = "2026.10.19.5", strings.Repeat("a", 40)
	t.Cleanup(func() { buildinfo.Version, buildinfo.Commit = previous[0], previous[1] })
	router := NewRouter(Options{Config: config.Default(), Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/server-version", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Data struct{ Version, Commit string } `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Data.Version != "2026.10.19.5" || body.Data.Commit != strings.Repeat("a", 40) {
		t.Fatalf("body = %s err=%v", rec.Body.String(), err)
	}
}
