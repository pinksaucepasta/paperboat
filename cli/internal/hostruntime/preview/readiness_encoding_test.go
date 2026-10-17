package preview

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestHTTPReadinessPreservesStrongETagAcrossCompressedProxy(t *testing.T) {
	lease := testDispatchRequest(t, time.Now().UTC()).Lease()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		observed := sessionReadyLease(lease)
		if r.Method == http.MethodPost {
			observed.State, observed.EdgeState = "connecting", "pending"
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("ETag", formatLeaseETag(lease.ID, 2))
		// Compression intermediaries weaken representation validators. The
		// ordinary HTTP transport advertises gzip unless identity is requested.
		if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			w.Header().Set("ETag", "W/"+formatLeaseETag(lease.ID, 2))
			w.Header().Set("Content-Encoding", "gzip")
			compressed := gzip.NewWriter(w)
			defer compressed.Close()
			_ = json.NewEncoder(compressed).Encode(map[string]any{"data": leaseToAPI(observed)})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": leaseToAPI(observed)})
	}))
	defer server.Close()
	base, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	observer, err := NewHTTPDispatchReadinessObserver(HTTPDispatchReadinessObserverConfig{
		ControlURL: server.URL, AllowedHosts: []string{base.Hostname()},
		Identities: readinessTokenSource("machine-identity"),
		Proofs:     &readinessProofSource{t: t, operation: "operation_dispatch_1", path: "/v1/previews/prv_dispatch_1/readiness"},
		Transport:  server.Client().Transport,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	ready, err := observer.ObservePreviewReadiness(ctx, DispatchReadiness{OperationID: "operation_dispatch_1", IdempotencyKey: "create-key", RequestID: "request_dispatch_1", CorrelationID: "correlation_dispatch_1"}, lease, 1)
	if err != nil || !isReadyLease(ready) || ready.Generation != 2 {
		t.Fatalf("readiness lost its strong lease validator through the proxy: %v", err)
	}
}
