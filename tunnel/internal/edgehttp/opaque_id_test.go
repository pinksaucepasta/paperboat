package edgehttp

import (
	"context"
	edgetelemetry "github.com/pinksaucepasta/paperboat-tunnel/internal/telemetry"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestTelemetryAdapterPreservesOpaqueIdentity(t *testing.T) {
	for _, id := range []string{"5b8a43c1-574f-4c56-94e1-a1adf6fc2c83", "rte_existing_01", "pvc_rte_0123456789abcdef", "asn_0123456789abcdef"} {
		if got := safeRequestID(id); got != id {
			t.Errorf("identity dropped: %q -> %q", id, got)
		}
	}
	for _, id := range []string{"customer.example.com", "Bearer_secret", "pbce_abcdefghijklmnop", "pbnative_abcdefghijklmnop", "PBCE_abcdefghijklmnop", "token_5b8a43c1-574f-4c56-94e1-a1adf6fc2c83", "ghp_abcdefghijklmnopqrstuvwxyz", "route\nforged"} {
		if got := safeRequestID(id); got != "" {
			t.Errorf("unsafe identity retained: %q", got)
		}
	}
}

func TestRequestIdentityPropagatesAndIgnoresUntrustedHeader(t *testing.T) {
	producer, err := NewRequestTelemetry(RequestTelemetryConfig{})
	if err != nil {
		t.Fatal(err)
	}
	var seen []RequestInfo
	handler := producer.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		info, ok := RequestTelemetryInfo(r.Context())
		if !ok {
			t.Fatal("request identity missing from context")
		}
		parsed, err := uuid.Parse(strings.TrimPrefix(info.IDs.RequestID, "request_"))
		if err != nil || parsed.Version() != 4 || parsed.Variant() != uuid.RFC4122 || "request_"+parsed.String() != info.IDs.RequestID {
			t.Fatalf("request ID is not canonical UUIDv4: %q", info.IDs.RequestID)
		}
		if got := requestIDFor(r); got != info.IDs.RequestID {
			t.Fatalf("carrier request identity changed: %q", got)
		}
		correlation, correlationErr := uuid.Parse(strings.TrimPrefix(info.CorrelationID, "correlation_"))
		if !edgetelemetry.SafeOpaqueID(info.CorrelationID) || correlationErr != nil || correlation.Version() != 4 || correlation.Variant() != uuid.RFC4122 || "correlation_"+correlation.String() != info.CorrelationID {
			t.Fatalf("correlation invalid: %q", info.CorrelationID)
		}
		seen = append(seen, info)
	}))
	for range 2 {
		request := httptest.NewRequest(http.MethodGet, "https://example.test/", nil)
		request.Header.Set("X-Request-ID", "client-chosen")
		handler.ServeHTTP(httptest.NewRecorder(), request)
	}
	if seen[0].IDs.RequestID == seen[1].IDs.RequestID || seen[0].CorrelationID == seen[1].CorrelationID {
		t.Fatal("independent requests reused identity")
	}
	id := "request_" + uuid.NewString()
	request := httptest.NewRequest(http.MethodGet, "https://example.test/", nil).WithContext(WithRequestTelemetryInfo(context.Background(), RequestInfo{IDs: edgetelemetry.SafeIDs{RequestID: id}}))
	if got := requestIDFor(request); got != id {
		t.Fatalf("trusted identity not preserved: %q", got)
	}
	policy := &Policy{}
	generated, err := policy.trustedRequestID()
	parsed, parseErr := uuid.Parse(strings.TrimPrefix(generated, "request_"))
	if err != nil || parseErr != nil || parsed.Version() != 4 || parsed.Variant() != uuid.RFC4122 || "request_"+parsed.String() != generated {
		t.Fatalf("gateway UUID: %q, %v", generated, err)
	}
}
