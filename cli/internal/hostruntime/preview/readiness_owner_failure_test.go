package preview

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/diagnostics"
)

func TestReadinessOwnerFailuresSurviveMutationAndRetryExhaustion(t *testing.T) {
	for _, scenario := range []struct {
		name, reason string
		posts, reads int
	}{
		{"invalid owner lease", "readiness_owner_validate", 1, 0},
		{"publication read retry exhaustion", "readiness_get_json_decode", 4, 4},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			admitted := testDispatchRequest(t, time.Now().UTC()).Lease()
			var mu sync.Mutex
			posts, reads, generation := 0, 0, admitted.Generation
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				if r.Method == http.MethodGet {
					reads++
					_, _ = w.Write([]byte(`{"data":{"id":["PRIVATE_RESPONSE_CANARY"]}}`))
					return
				}
				posts++
				if r.Header.Get("If-Match") != formatLeaseETag(admitted.ID, generation) {
					t.Error("readiness did not use the real session's adopted generation")
					w.WriteHeader(http.StatusPreconditionFailed)
					return
				}
				generation++
				observed := sessionReadyLease(admitted)
				if scenario.reads > 0 {
					observed.State, observed.EdgeState = "connecting", "pending"
				} else {
					observed.LeaseDeadline = time.Now().UTC().Add(-time.Second)
				}
				w.Header().Set("ETag", formatLeaseETag(admitted.ID, generation))
				_ = json.NewEncoder(w).Encode(map[string]any{"data": leaseToAPI(observed)})
			}))
			defer server.Close()
			endpoint, _ := url.Parse(server.URL)
			observer, err := NewHTTPDispatchReadinessObserver(HTTPDispatchReadinessObserverConfig{
				ControlURL: server.URL, AllowedHosts: []string{endpoint.Hostname()},
				Identities: readinessTokenSource("PRIVATE_AUTH_CANARY"),
				Proofs:     &readinessProofSource{t: t, operation: "operation_dispatch_1", path: "/v1/previews/prv_dispatch_1/readiness"}, Transport: server.Client().Transport,
			})
			if err != nil {
				t.Fatal(err)
			}
			carrier := &readinessCarrier{admitted: admitted, observer: observer, readiness: DispatchReadiness{OperationID: "operation_dispatch_1", IdempotencyKey: "create-key", RequestID: "request_dispatch_1", CorrelationID: "correlation_dispatch_1"}, inner: &sessionCarrier{run: func(ctx context.Context, lease Lease, ready func(Lease) error) error {
				if err := ready(sessionReadyLease(lease)); err != nil {
					return err
				}
				<-ctx.Done()
				return nil
			}}}
			recorder := diagnostics.NewMemoryRecorder()
			session, err := StartExisting(diagnostics.WithRecorder(t.Context(), recorder), SessionConfig{LeaseClient: &sessionLeaseClient{}, Carrier: carrier, OwnerMachineID: admitted.OwnerMachineID, OwnerSessionID: admitted.OwnerSessionID, OwnerSessionKind: admitted.OwnerSessionKind, Target: admitted.Target, AccessMode: admitted.AccessMode, DisableParentWatch: true}, admitted)
			if err != nil {
				t.Fatal(err)
			}
			defer session.Stop(context.Background())
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			if _, err := session.WaitReady(ctx); err == nil {
				t.Fatal("invalid publication became ready")
			}
			failure := session.Wait()
			if scenario.reads > 0 {
				if !errors.Is(failure, ErrDispatchUnavailable) || !errors.Is(failure, ErrDispatchInvalid) {
					t.Fatalf("retry exhaustion lost the original rejection: %v", failure)
				}
			} else if !errors.Is(failure, ErrSessionInvalid) {
				t.Fatalf("owner validation lost its failure class: %v", failure)
			}
			manager := &DispatchManager{operations: map[string]dispatchOperation{"operation_01": {hash: "hash_01", session: session}}}
			manager.observeSession("operation_01", "hash_01", session, false)
			events := recorder.Recent()
			if len(events) != 1 || events[0].Fields["reason"] != scenario.reason {
				t.Fatalf("owner stage missing: %+v", events)
			}
			if scenario.reads > 0 && events[0].Fields["retry_class"] != "readiness_generation_race_exhausted" {
				t.Fatalf("retry exhaustion missing: %+v", events)
			}
			mu.Lock()
			gotPosts, gotReads := posts, reads
			mu.Unlock()
			if gotPosts != scenario.posts || gotReads != scenario.reads {
				t.Fatalf("policy changed: POST=%d GET=%d, want %d/%d", gotPosts, gotReads, scenario.posts, scenario.reads)
			}
			encoded, _ := json.Marshal(events)
			if strings.Contains(string(encoded), "PRIVATE_") {
				t.Fatalf("private owner failure escaped: %s", encoded)
			}
		})
	}
}
