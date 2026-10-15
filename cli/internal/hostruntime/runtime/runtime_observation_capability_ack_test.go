//go:build darwin || linux || windows

package runtime

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestRuntimeObservationConfirmsAppliedCapabilitiesBeforeReceipt(t *testing.T) {
	for _, confirmStatus := range []int{http.StatusOK, http.StatusServiceUnavailable, http.StatusAccepted} {
		t.Run(http.StatusText(confirmStatus), func(t *testing.T) {
			var accepted atomic.Bool
			var calls atomic.Int32
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/runtime-observations" {
					t.Error("unexpected request path")
					w.WriteHeader(404)
					return
				}
				n := calls.Add(1)
				var body struct {
					Capabilities *machineCapabilitiesObservation `json:"machine_capabilities"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
					w.WriteHeader(400)
					return
				}
				if n > 2 {
					t.Error("capability confirmation exceeded its request bound")
					w.WriteHeader(503)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				if n == 2 {
					if body.Capabilities == nil || body.Capabilities.Version != 1 || !body.Capabilities.Applied.ManagedSSH || body.Capabilities.Status != "applied" {
						t.Error("applied SSH capability not sent to server")
					}
					if confirmStatus == http.StatusServiceUnavailable {
						w.WriteHeader(confirmStatus)
						return
					}
					if confirmStatus == http.StatusAccepted {
						w.WriteHeader(confirmStatus)
						_, _ = w.Write([]byte(`{"data":{"machine_capabilities":{"schema":"paperboat.machine-capabilities/v1","desired_version":1,"desired":{"managed_ssh":true}},"auxiliary_rejections":[{"observation":"machine_capabilities","code":"capabilities_observation_unavailable"}]}}`))
						return
					}
					accepted.Store(true)
				}
				_, _ = w.Write([]byte(`{"data":{"machine_capabilities":{"schema":"paperboat.machine-capabilities/v1","desired_version":1,"desired":{"managed_ssh":true}}}}`))
			}))
			defer backend.Close()
			sender := &runtimeObservationSender{endpoint: backend.URL + "/v1/runtime-observations", tokens: livenessObservationTokenSource{}, proofs: livenessObservationProofSource{}, operationID: func() (string, error) { return "operation_capabilities", nil }, environmentID: "environment_fixture", machineID: "machine_fixture", reporterVersion: "test", workerGeneration: 1, client: backend.Client(), receiptPath: filepath.Join(t.TempDir(), "runtime", "server-heartbeat.json"), capabilitiesController: newMachineCapabilityController(nil)}
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()
			err := sender.Send(ctx)
			if calls.Load() != 2 {
				t.Fatalf("server has not received applied capabilities: requests=%d", calls.Load())
			}
			_, receiptErr := os.Stat(sender.receiptPath)
			if confirmStatus == http.StatusOK {
				if err != nil || !accepted.Load() || receiptErr != nil {
					t.Fatalf("applied acknowledgement=%t receipt=%v send=%v", accepted.Load(), receiptErr, err)
				}
			} else if err == nil || !os.IsNotExist(receiptErr) {
				t.Fatalf("failed capability acknowledgement published readiness: receipt=%v send=%v", receiptErr, err)
			}
		})
	}
}
