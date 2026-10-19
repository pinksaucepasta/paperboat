//go:build darwin || linux || windows

package runtime

import (
	"context"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/machineservices"
)

// policyObservationTransport accepts observations and answers with a policy that allows automatic
// discovery, as the server does once the person has enabled it.
type policyObservationTransport struct{ calls atomic.Int32 }

func (t *policyObservationTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	t.calls.Add(1)
	if request.Body != nil {
		_, _ = io.Copy(io.Discard, request.Body)
		_ = request.Body.Close()
	}
	body := `{"data":{"machine_services_policy":{"generation":1,"automatic_discovery_enabled":true}}}`
	return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: request}, nil
}

// A service that starts listening is announced within the watch interval, not at the next periodic
// observation, so other computers can reach it without waiting.
func TestRuntimeObservationAnnouncesANewServiceWithoutWaitingForTheNextObservation(t *testing.T) {
	transport := &policyObservationTransport{}
	var listening atomic.Bool
	sender := &runtimeObservationSender{
		endpoint:         "https://observations.invalid/v1/runtime-observations",
		tokens:           livenessObservationTokenSource{},
		proofs:           livenessObservationProofSource{},
		operationID:      func() (string, error) { return "op_runtime_observation_services_0001", nil },
		environmentID:    "env_services",
		machineID:        "machine_services",
		reporterVersion:  "test",
		client:           &http.Client{Transport: transport},
		receiptPath:      filepath.Join(t.TempDir(), "runtime", "server-heartbeat.json"),
		workerGeneration: 1,
		osBootID:         "boot-services",
		lazyBootID:       "boot-services", lazyStartedAt: time.Now(), installationGeneration: 7,
		machineServicesDiscover: func(context.Context) ([]machineservices.Service, error) {
			if listening.Load() {
				return []machineservices.Service{{Port: 18311, Loopback: "127.0.0.1"}}, nil
			}
			return nil, nil
		},
	}
	service := &runtimeObservationService{sender: sender, interval: time.Hour, timeout: time.Second, servicesWatch: 20 * time.Millisecond}
	if err := service.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = service.Shutdown(shutdownCtx)
	}()
	time.Sleep(100 * time.Millisecond)
	before := transport.calls.Load()
	listening.Store(true)
	deadline := time.Now().Add(2 * time.Second)
	for transport.calls.Load() <= before {
		if time.Now().After(deadline) {
			t.Fatalf("a new listening port was not announced (calls=%d)", transport.calls.Load())
		}
		time.Sleep(5 * time.Millisecond)
	}
}
