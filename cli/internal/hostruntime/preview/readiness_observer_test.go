package preview

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type readinessRoundTripper func(*http.Request) (*http.Response, error)

func (f readinessRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

type readinessTokenSource string

func (s readinessTokenSource) Token(context.Context) (string, error) { return string(s), nil }

type readinessProofSource struct {
	t         *testing.T
	body      []byte
	path      string
	operation string
	method    string
}

func (s *readinessProofSource) Proof(_ context.Context, operation, method, path string, body []byte) ([]byte, error) {
	s.t.Helper()
	if operation != s.operation || (method != http.MethodPost && method != http.MethodGet) || path != s.path {
		s.t.Fatalf("proof binding operation=%q method=%q path=%q", operation, method, path)
	}
	s.method = method
	s.body = append([]byte(nil), body...)
	return []byte("signed-machine-proof"), nil
}

func TestHTTPDispatchReadinessObserverBindsMachineProofAndReturnsCanonicalLease(t *testing.T) {
	now := time.Date(2098, 1, 2, 3, 4, 5, 0, time.UTC)
	lease := testDispatchRequest(t, now).Lease()
	proofs := &readinessProofSource{t: t, operation: "operation_dispatch_1", path: "/v1/previews/prv_dispatch_1/readiness"}
	transport := readinessRoundTripper(func(request *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatal(err)
		}
		if string(body) != string(proofs.body) || request.Header.Get("Authorization") != "Bearer machine-identity" || request.Header.Get("X-Paperboat-Machine-Identity") != "machine-identity" || request.Header.Get("X-Paperboat-Machine-Proof") != base64.RawURLEncoding.EncodeToString([]byte("signed-machine-proof")) || request.Header.Get("If-Match") != lease.ETag || request.Header.Get("Idempotency-Key") != "operation_dispatch_1" || request.Header.Get("Request-Id") != "request_dispatch_1" || request.Header.Get("Correlation-Id") != "correlation_dispatch_1" {
			t.Fatalf("unexpected readiness request headers=%v body=%s proofBody=%s", request.Header, body, proofs.body)
		}
		ready := lease
		ready.State, ready.AllocationState, ready.EdgeState, ready.OriginState = "ready", "ready", "ready", "ready"
		ready.ETag = ""
		ready.Generation = 0
		encoded, err := json.Marshal(map[string]any{"data": ready})
		if err != nil {
			t.Fatal(err)
		}
		headers := make(http.Header)
		headers.Set("ETag", formatLeaseETag(lease.ID, 2))
		return &http.Response{StatusCode: http.StatusOK, Header: headers, Body: io.NopCloser(strings.NewReader(string(encoded)))}, nil
	})
	observer, err := NewHTTPDispatchReadinessObserver(HTTPDispatchReadinessObserverConfig{ControlURL: "https://api.example.test", AllowedHosts: []string{"api.example.test"}, Identities: readinessTokenSource("machine-identity"), Proofs: proofs, Transport: transport})
	if err != nil {
		t.Fatal(err)
	}
	if observer.client.Timeout != 15*time.Second {
		t.Fatalf("readiness timeout=%s", observer.client.Timeout)
	}
	ready, err := observer.ObservePreviewReadiness(context.Background(), DispatchReadiness{OperationID: "operation_dispatch_1", IdempotencyKey: "create-key", RequestID: "request_dispatch_1", CorrelationID: "correlation_dispatch_1"}, lease, 1)
	if err != nil {
		t.Fatal(err)
	}
	if ready.Generation != 2 || ready.ETag != formatLeaseETag(lease.ID, 2) || !isReadyLease(ready) {
		t.Fatalf("ready lease=%+v", ready)
	}
}

func TestHTTPDispatchReadinessObserverRejectsUnboundedConfiguration(t *testing.T) {
	_, err := NewHTTPDispatchReadinessObserver(HTTPDispatchReadinessObserverConfig{ControlURL: "https://api.example.test", AllowedHosts: []string{"api.example.test"}, Identities: readinessTokenSource("identity"), Proofs: &readinessProofSource{t: t}, Timeout: 31 * time.Second})
	if !errors.Is(err, ErrDispatchInvalid) {
		t.Fatalf("error=%v", err)
	}
}

func TestHTTPDispatchReadinessObserverRejectsRedirectOversizeDuplicateAndConflict(t *testing.T) {
	now := time.Date(2098, 1, 2, 3, 4, 5, 0, time.UTC)
	lease := testDispatchRequest(t, now).Lease()
	for name, testCase := range map[string]struct {
		response *http.Response
		want     error
	}{
		"conflict": {response: &http.Response{StatusCode: http.StatusConflict, Body: io.NopCloser(strings.NewReader(`{}`))}, want: ErrDispatchConflict},
		"oversize": {response: &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(strings.Repeat(" ", maxPreviewReadinessResponseBytes+1)))}, want: ErrDispatchUnavailable},
		"duplicate": {response: &http.Response{StatusCode: http.StatusOK, Header: func() http.Header {
			headers := make(http.Header)
			headers.Set("ETag", formatLeaseETag(lease.ID, 2))
			return headers
		}(), Body: io.NopCloser(strings.NewReader(`{"data":{},"data":{}}`))}, want: ErrDispatchInvalid},
	} {
		t.Run(name, func(t *testing.T) {
			observer, err := NewHTTPDispatchReadinessObserver(HTTPDispatchReadinessObserverConfig{ControlURL: "https://api.example.test", AllowedHosts: []string{"api.example.test"}, Identities: readinessTokenSource("machine-identity"), Proofs: &readinessProofSource{t: t, operation: "operation_dispatch_1", path: "/v1/previews/prv_dispatch_1/readiness"}, Transport: readinessRoundTripper(func(*http.Request) (*http.Response, error) { return testCase.response, nil })})
			if err != nil {
				t.Fatal(err)
			}
			_, err = observer.ObservePreviewReadiness(context.Background(), DispatchReadiness{OperationID: "operation_dispatch_1", IdempotencyKey: "create-key", RequestID: "request_dispatch_1", CorrelationID: "correlation_dispatch_1"}, lease, 1)
			if !errors.Is(err, testCase.want) {
				t.Fatalf("error=%v want %v", err, testCase.want)
			}
		})
	}
	redirecting := readinessRoundTripper(func(request *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusTemporaryRedirect, Header: http.Header{"Location": []string{"https://evil.example.test"}}, Body: io.NopCloser(strings.NewReader(`{}`)), Request: request}, nil
	})
	observer, err := NewHTTPDispatchReadinessObserver(HTTPDispatchReadinessObserverConfig{ControlURL: "https://api.example.test", AllowedHosts: []string{"api.example.test"}, Identities: readinessTokenSource("machine-identity"), Proofs: &readinessProofSource{t: t, operation: "operation_dispatch_1", path: "/v1/previews/prv_dispatch_1/readiness"}, Transport: redirecting})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := observer.ObservePreviewReadiness(context.Background(), DispatchReadiness{OperationID: "operation_dispatch_1", IdempotencyKey: "create-key", RequestID: "request_dispatch_1", CorrelationID: "correlation_dispatch_1"}, lease, 1); !errors.Is(err, ErrDispatchInvalid) {
		t.Fatalf("redirect error=%v", err)
	}
}

func TestHTTPDispatchReadinessWaitsForSignedPublicationRead(t *testing.T) {
	for _, state := range []string{"ready", "revoked", "foreign", "canceled", "stale"} {
		t.Run(state, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			lease := testDispatchRequest(t, time.Now().UTC()).Lease()
			lease.Endpoint, lease.EndpointState = "", "custom_domain_not_ready"
			proofs := &readinessProofSource{t: t, operation: "operation_dispatch_1", path: "/v1/previews/prv_dispatch_1/readiness"}
			calls := 0
			transport := readinessRoundTripper(func(request *http.Request) (*http.Response, error) {
				calls++
				observed := lease
				observed.State = "connecting"
				observed.AllocationState, observed.EdgeState, observed.OriginState = "ready", "ready", "ready"
				if calls == 1 {
					if request.Method != http.MethodPost {
						t.Fatal("readiness was not reported before polling")
					}
				} else {
					body, err := io.ReadAll(request.Body)
					if err != nil {
						t.Fatal(err)
					}
					if request.Method != http.MethodGet || proofs.method != http.MethodGet || len(body) != 0 || len(proofs.body) != 0 || request.Header.Get("If-Match") != "" || request.Header.Get("Idempotency-Key") != "" || request.Header.Get("X-Paperboat-Machine-Identity") != "machine-identity" || request.Header.Get("X-Paperboat-Machine-Proof") == "" {
						t.Fatal("publication read was not independently machine-authenticated")
					}
					observed.State, observed.EndpointState, observed.Endpoint = "ready", "ready", "https://published.example.test"
					if state == "revoked" {
						observed.State = "revoked"
					}
					if state == "foreign" {
						observed.OwnerMachineID = "foreign-machine"
					}
					if state == "canceled" {
						observed.State = "connecting"
						observed.EndpointState = "custom_domain_not_ready"
						observed.Endpoint = ""
						cancel()
					}
				}
				data, err := json.Marshal(map[string]any{"data": observed})
				if err != nil {
					t.Fatal(err)
				}
				headers := make(http.Header)
				generation := int64(calls + 1)
				if state == "stale" && calls > 1 {
					generation = 1
				}
				headers.Set("ETag", formatLeaseETag(lease.ID, generation))
				return &http.Response{StatusCode: http.StatusOK, Header: headers, Body: io.NopCloser(strings.NewReader(string(data)))}, nil
			})
			observer, err := NewHTTPDispatchReadinessObserver(HTTPDispatchReadinessObserverConfig{ControlURL: "https://api.example.test", AllowedHosts: []string{"api.example.test"}, Identities: readinessTokenSource("machine-identity"), Proofs: proofs, Transport: transport})
			if err != nil {
				t.Fatal(err)
			}
			ready, err := observer.ObservePreviewReadiness(ctx, DispatchReadiness{OperationID: "operation_dispatch_1", IdempotencyKey: "create-key", RequestID: "request_dispatch_1", CorrelationID: "correlation_dispatch_1"}, lease, 1)
			if state == "ready" {
				if err != nil || !isReadyLease(ready) || ready.Endpoint != "https://published.example.test" || calls != 2 {
					t.Fatalf("pending publication escaped: calls=%d ready=%v err=%v", calls, isReadyLease(ready), err)
				}
			} else if state == "canceled" {
				if !errors.Is(err, context.Canceled) || calls != 2 {
					t.Fatalf("cancellation did not stop publication polling: calls=%d err=%v", calls, err)
				}
			} else if !errors.Is(err, ErrDispatchInvalid) || calls != 2 {
				t.Fatalf("unsafe publication accepted: calls=%d err=%v", calls, err)
			}
		})
	}
}
