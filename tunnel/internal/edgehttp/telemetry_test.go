package edgehttp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	edgetelemetry "github.com/pinksaucepasta/paperboat-tunnel/internal/telemetry"
)

type telemetryRoundTripFunc func(*http.Request) (*http.Response, error)

func (f telemetryRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func newTestRequestTelemetry(t *testing.T) (*RequestTelemetry, *edgetelemetry.Metrics, *edgetelemetry.EventLog) {
	t.Helper()
	metrics := edgetelemetry.NewMetrics()
	events, err := edgetelemetry.NewEventLogWithQueue(128, 128)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = events.Close() })
	producer, err := NewRequestTelemetry(RequestTelemetryConfig{
		Metrics: metrics,
		Events:  events,
		Clock:   func() time.Time { return time.Date(2026, 8, 31, 1, 2, 3, 0, time.UTC) },
		Info: RequestInfo{
			IDs:           edgetelemetry.SafeIDs{TunnelID: "tunnel_1", RouteID: "route_1", ConnectorID: "connector_1"},
			Generations:   edgetelemetry.Generations{Route: 4, Connector: 2},
			CorrelationID: "corr_edge_test_1",
			RouteKind:     "tunnel_https_wss",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return producer, metrics, events
}

func TestRequestTelemetryRoundTripCountsStreamingBytesAndTerminalOnce(t *testing.T) {
	producer, metrics, events := newTestRequestTelemetry(t)
	transport := producer.RoundTripper(telemetryRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		if _, err := io.Copy(io.Discard, request.Body); err != nil {
			return nil, err
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			ProtoMajor: 1,
			ProtoMinor: 1,
			Body:       io.NopCloser(strings.NewReader("origin-response")),
			Header:     make(http.Header),
			Request:    request,
		}, nil
	}))
	request := httptest.NewRequest(http.MethodPost, "https://secret.example.test/private?token=secret-query", strings.NewReader("client-request"))
	request.Header.Set("Authorization", "Bearer secret-header")
	response, err := transport.RoundTrip(request)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "origin-response" {
		t.Fatalf("response body = %q", body)
	}
	if err := response.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if err := response.Body.Close(); err != nil {
		t.Fatal(err)
	}

	samples := metrics.Snapshot()
	if got := metricValue(samples, edgetelemetry.MetricTrafficBytes, "ingress"); got != uint64(len("client-request")) {
		t.Fatalf("ingress bytes = %d", got)
	}
	if got := metricValue(samples, edgetelemetry.MetricTrafficBytes, "egress"); got != uint64(len("origin-response")) {
		t.Fatalf("egress bytes = %d", got)
	}
	if got := metricValue(samples, edgetelemetry.MetricRouteRequests, "tunnel_https_wss", "success"); got != 1 {
		t.Fatalf("route success count = %d", got)
	}
	if got := metricValue(samples, edgetelemetry.MetricOriginConnections, "http1", "success"); got != 1 {
		t.Fatalf("origin success count = %d", got)
	}
	eventCount := 0
	for _, event := range events.Snapshot() {
		if event.Name == requestEventCompleted {
			eventCount++
		}
		encoded, _ := json.Marshal(event)
		if bytes.Contains(encoded, []byte("secret")) {
			t.Fatalf("event leaked secret: %s", encoded)
		}
	}
	if eventCount != 1 {
		t.Fatalf("completed events = %d", eventCount)
	}
}

func TestRequestTelemetryFailureTimeoutAndUpgradeLabels(t *testing.T) {
	producer, metrics, events := newTestRequestTelemetry(t)
	timeout := timeoutError{}
	transport := producer.RoundTripper(telemetryRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, timeout
	}))
	request := httptest.NewRequest(http.MethodGet, "http://origin.invalid/", nil)
	if _, err := transport.RoundTrip(request); !errors.Is(err, timeout) {
		t.Fatalf("RoundTrip error = %v", err)
	}

	upgradeTransport := producer.RoundTripper(telemetryRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusSwitchingProtocols,
			ProtoMajor: 1,
			Body:       io.NopCloser(strings.NewReader("websocket-bytes")),
			Header:     make(http.Header),
			Request:    request,
		}, nil
	}))
	upgrade := httptest.NewRequest(http.MethodGet, "http://origin.invalid/socket", nil)
	upgrade.Header.Set("Connection", "Upgrade")
	upgrade.Header.Set("Upgrade", "websocket")
	response, err := upgradeTransport.RoundTrip(upgrade)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(response.Body)
	_ = response.Body.Close()

	samples := metrics.Snapshot()
	if got := metricValue(samples, edgetelemetry.MetricOriginConnections, "http1", "timeout"); got != 1 {
		t.Fatalf("origin timeout count = %d", got)
	}
	if got := metricValue(samples, edgetelemetry.MetricOriginFailures, "http1", "timeout"); got != 1 {
		t.Fatalf("origin timeout failure count = %d", got)
	}
	if got := metricValue(samples, edgetelemetry.MetricProtocolUpgrades, "http1", "websocket"); got != 1 {
		t.Fatalf("upgrade count = %d", got)
	}
	if got := metricValue(samples, edgetelemetry.MetricRouteRequests, "tunnel_https_wss", "success"); got != 1 {
		t.Fatalf("websocket route success count = %d", got)
	}
	for _, event := range events.Snapshot() {
		encoded, _ := json.Marshal(event)
		if bytes.Contains(encoded, []byte("origin.invalid")) || bytes.Contains(encoded, []byte("timeout")) && bytes.Contains(encoded, []byte("Authorization")) {
			t.Fatalf("event contains request data: %s", encoded)
		}
	}
}

func TestRequestTelemetryHandlerPreservesStatusTrailersAndCancellation(t *testing.T) {
	producer, metrics, _ := newTestRequestTelemetry(t)
	handler := producer.Handler(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if _, err := io.ReadAll(request.Body); err != nil {
			t.Errorf("read request: %v", err)
		}
		writer.Header().Set("Trailer", "X-Stream-Trailer")
		writer.WriteHeader(http.StatusCreated)
		_, _ = writer.Write([]byte("handler-response"))
		writer.Header().Set("X-Stream-Trailer", "done")
	}))
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "http://edge.invalid/upload", strings.NewReader("handler-request"))
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusCreated || response.Body.String() != "handler-response" {
		t.Fatalf("response = %d %q", response.Code, response.Body.String())
	}
	if response.Result().Trailer.Get("X-Stream-Trailer") != "done" {
		t.Fatalf("trailer = %q", response.Result().Trailer.Get("X-Stream-Trailer"))
	}
	if got := metricValue(metrics.Snapshot(), edgetelemetry.MetricEdgeRequests, "http", "success"); got != 1 {
		t.Fatalf("handler success count = %d", got)
	}
}

func TestRequestTelemetryCancellationClosesResponseAndRecordsOnce(t *testing.T) {
	producer, metrics, events := newTestRequestTelemetry(t)
	body := newBlockingResponseBody()
	transport := producer.RoundTripper(telemetryRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, ProtoMajor: 2, Body: body, Header: make(http.Header), Request: request}, nil
	}))
	ctx, cancel := context.WithCancel(context.Background())
	request := httptest.NewRequest(http.MethodGet, "https://edge.invalid/grpc", nil).WithContext(ctx)
	request.Header.Set("Content-Type", "application/grpc")
	response, err := transport.RoundTrip(request)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	readDone := make(chan error, 1)
	go func() {
		_, readErr := response.Body.Read(make([]byte, 8))
		readDone <- readErr
	}()
	select {
	case <-readDone:
	case <-time.After(time.Second):
		t.Fatal("response body did not close after cancellation")
	}
	_ = response.Body.Close()
	if got := metricValue(metrics.Snapshot(), edgetelemetry.MetricRouteRequests, "tunnel_https_wss", "canceled"); got != 1 {
		t.Fatalf("canceled route count = %d", got)
	}
	eventsForRequest := 0
	for _, event := range events.Snapshot() {
		if event.Name == requestEventCanceled {
			eventsForRequest++
		}
	}
	if eventsForRequest != 1 {
		t.Fatalf("canceled events = %d", eventsForRequest)
	}
}

type timeoutError struct{}

func (timeoutError) Error() string   { return "timeout secret should never be emitted" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

type blockingResponseBody struct {
	closed chan struct{}
	once   sync.Once
}

func newBlockingResponseBody() *blockingResponseBody {
	return &blockingResponseBody{closed: make(chan struct{})}
}

func (b *blockingResponseBody) Read([]byte) (int, error) {
	<-b.closed
	return 0, io.ErrClosedPipe
}

func (b *blockingResponseBody) Close() error {
	b.once.Do(func() { close(b.closed) })
	return nil
}

func metricValue(samples []edgetelemetry.MetricSample, name string, labels ...string) uint64 {
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

func TestRequestTelemetryMixedFailuresAndDeadlineRemainFailures(t *testing.T) {
	for _, tc := range []struct {
		name     string
		cause    error
		canceled bool
		outcome  string
	}{
		{"pure cancel", context.Canceled, true, "canceled"},
		{"refused plus cancel", errors.Join(syscall.ECONNREFUSED, context.Canceled), true, "failed"},
		{"EOF plus IO", errors.Join(io.EOF, syscall.EIO), false, "failed"},
		{"deadline plus cancel", errors.Join(context.DeadlineExceeded, context.Canceled), true, "failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			producer, metrics, events := newTestRequestTelemetry(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.canceled {
				cancel()
			}
			request := httptest.NewRequest(http.MethodGet, "http://private.example.test", nil).WithContext(ctx)
			_, err := producer.RoundTripper(telemetryRoundTripFunc(func(*http.Request) (*http.Response, error) { return nil, tc.cause })).RoundTrip(request)
			if err != tc.cause {
				t.Fatal("original cause replaced")
			}
			if metricValue(metrics.Snapshot(), edgetelemetry.MetricRouteRequests, "tunnel_https_wss", tc.outcome) != 1 {
				t.Fatal("terminal outcome lost")
			}
			for _, event := range events.Snapshot() {
				if event.Name == requestEventFailed && event.Severity != edgetelemetry.SeverityError {
					t.Fatal("failure severity lost")
				}
			}
		})
	}
}

func TestRequestTelemetryImmediateCancellationJoinsResponseCleanup(t *testing.T) {
	producer, _, _ := newTestRequestTelemetry(t)
	for range 32 {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		body := newBlockingResponseBody()
		wrapped := newResponseBody(ctx, body, producer.startRequest(ctx, RequestInfo{}, "http", false))
		if err := wrapped.Close(); err != nil {
			t.Fatal(err)
		}
		select {
		case <-body.closed:
		case <-time.After(time.Second):
			t.Fatal("response did not close")
		}
	}
}

func TestRequestTelemetryPanicIsFailureAndPreservesAbortBoundary(t *testing.T) {
	producer, metrics, _ := newTestRequestTelemetry(t)
	func() {
		defer func() {
			if recover() != "PRIVATE_panic" {
				t.Fatal("panic contract changed")
			}
		}()
		producer.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("PRIVATE_panic") })).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "http://private.example.test", nil))
	}()
	if metricValue(metrics.Snapshot(), edgetelemetry.MetricEdgeRequests, "http", "failed") != 1 || metricValue(metrics.Snapshot(), edgetelemetry.MetricEdgeRequests, "http", "success") != 0 {
		t.Fatal("panic counted as success")
	}
}

type privateProxyCause struct{ calls int }

func (e *privateProxyCause) Error() string { e.calls++; return "PRIVATE origin credential" }
func (e *privateProxyCause) Unwrap() error { return syscall.EIO }

func TestProxyFailureOwnerPreservesCauseAndRecoveryWithoutFormatting(t *testing.T) {
	cause := &privateProxyCause{}
	calls := 0
	observed := 0
	type contextKey struct{}
	ctx := context.WithValue(context.Background(), contextKey{}, "borrowed")
	proxy := &httputil.ReverseProxy{Rewrite: func(pr *httputil.ProxyRequest) { pr.Out.URL.Scheme = "http"; pr.Out.URL.Host = "private.example.test" }, Transport: telemetryRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			return nil, cause
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("recovered")), Request: r}, nil
	})}
	configureProxyDiagnostics(proxy, func(got context.Context, err error) {
		observed++
		if got.Value(contextKey{}) != "borrowed" || !errors.Is(err, syscall.EIO) {
			t.Fatal("owner lost original cause/context")
		}
	})
	first := httptest.NewRecorder()
	proxy.ServeHTTP(first, httptest.NewRequest(http.MethodGet, "http://public.example.test", nil).WithContext(ctx))
	if first.Code != 502 || first.Body.String() != "Bad Gateway\n" || observed != 1 || cause.calls != 0 {
		t.Fatalf("status=%d observed=%d formatted=%d", first.Code, observed, cause.calls)
	}
	second := httptest.NewRecorder()
	proxy.ServeHTTP(second, httptest.NewRequest(http.MethodGet, "http://public.example.test", nil).WithContext(ctx))
	if second.Code != 200 || second.Body.String() != "recovered" || observed != 1 {
		t.Fatal("recovery duplicated failure")
	}
	proxy.ErrorHandler(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "http://public.example.test", nil), http.ErrAbortHandler)
	if observed != 1 {
		t.Fatal("explicit abort reported")
	}
	diagnosticObserved := 0
	configureProxyDiagnostics(proxy, func(_ context.Context, err error) {
		diagnosticObserved++
		if err != errProxyHTTPDiagnostic {
			t.Fatal("raw diagnostic retained")
		}
	})
	proxy.ErrorLog.Print("PRIVATE formatted message")
	if diagnosticObserved != 1 {
		t.Fatal("diagnostic omitted")
	}
}
