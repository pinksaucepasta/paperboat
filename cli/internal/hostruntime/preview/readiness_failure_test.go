package preview

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/diagnostics"
	"github.com/pinksaucepasta/paperboat/internal/supportref"
)

func TestDispatchRetainsReadinessValidationStageWithoutPrivateData(t *testing.T) {
	for _, method := range []string{http.MethodPost, http.MethodGet} {
		for _, stage := range []string{"http_status", "json_decode", "etag_invalid", "identity_changed", "stale_generation", "lease_state_invalid", "network"} {
			t.Run(method+"/"+stage, func(t *testing.T) { testReadinessFailureDiagnostic(t, method, stage) })
		}
	}
}

func testReadinessFailureDiagnostic(t *testing.T, method, stage string) {
	t.Helper()
	lease := testDispatchRequest(t, time.Now().UTC()).Lease()
	status := http.StatusOK
	if stage == "http_status" {
		status = http.StatusBadGateway
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		observed := sessionReadyLease(lease)
		generation := int64(2)
		if r.Method != method {
			observed.State, observed.EdgeState = "connecting", "pending"
		} else {
			switch stage {
			case "network":
				connection, _, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Error(err)
					return
				}
				_ = connection.Close()
				return
			case "json_decode":
				w.Header().Set("ETag", formatLeaseETag(lease.ID, generation))
				_, _ = w.Write([]byte(`{"data":{"id":["PRIVATE_RESPONSE_CANARY"]}}`))
				return
			case "identity_changed":
				observed.OwnerMachineID = "foreign-machine"
			case "stale_generation":
				generation = 1
			case "lease_state_invalid":
				observed.State = "revoked"
			}
		}
		etag := formatLeaseETag(lease.ID, generation)
		if r.Method == method && stage == "etag_invalid" {
			etag = "W/" + etag
		}
		w.Header().Set("ETag", etag)
		if r.Method == method {
			w.WriteHeader(status)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": leaseToAPI(observed)})
	}))
	defer server.Close()
	endpoint, _ := url.Parse(server.URL)
	observer, err := NewHTTPDispatchReadinessObserver(HTTPDispatchReadinessObserverConfig{
		ControlURL: server.URL, AllowedHosts: []string{endpoint.Hostname()},
		Identities: readinessTokenSource("PRIVATE_AUTH_CANARY"),
		Proofs:     &readinessProofSource{t: t, operation: "operation_dispatch_1", path: "/v1/previews/prv_dispatch_1/readiness"},
		Transport:  server.Client().Transport,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	_, failure := observer.ObservePreviewReadiness(ctx, DispatchReadiness{OperationID: "operation_dispatch_1", IdempotencyKey: "create-key", RequestID: "request_dispatch_1", CorrelationID: "correlation_dispatch_1"}, lease, 1)
	if failure == nil {
		t.Fatal("invalid readiness response was accepted")
	}
	for _, ready := range []bool{false, true} {
		recorder := diagnostics.NewMemoryRecorder()
		reference := "support_01234567-89ab-4def-8123-456789abcdef"
		sessionCtx := supportref.WithContext(diagnostics.WithRecorder(t.Context(), recorder), reference)
		done := make(chan struct{})
		close(done)
		session := &Session{ctx: sessionCtx, done: done, readySet: ready, result: errors.Join(ErrLeaseLost, fmt.Errorf("PRIVATE_WRAPPER_CANARY: %w", failure))}
		manager := &DispatchManager{operations: map[string]dispatchOperation{"operation_01": {hash: "hash_01", session: session}}}
		manager.observeSession("operation_01", "hash_01", session, false)
		events := recorder.Recent()
		want := "readiness_" + strings.ToLower(method) + "_" + stage
		if len(events) != 1 || events[0].Code != "preview_dispatch_failed" || events[0].Fields["reason"] != want || events[0].SupportReference != reference {
			t.Fatalf("ready=%t: lost readiness stage %s: %+v", ready, want, events)
		}
		if stage != "network" && events[0].Fields["http_status"] != strconv.Itoa(status) {
			t.Fatalf("lost readiness HTTP status: %+v", events)
		}
		encoded, _ := json.Marshal(events)
		if strings.Contains(string(encoded), "PRIVATE_") || strings.Contains(string(encoded), "Authorization") {
			t.Fatalf("private readiness data escaped: %s", encoded)
		}
	}
}
