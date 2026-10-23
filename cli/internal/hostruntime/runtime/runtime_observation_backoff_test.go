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
)

// failingObservationTransport accepts the first observation (the start) and fails every later one, as a
// server that goes down after the machine started.
type failingObservationTransport struct {
	calls atomic.Int64
	start livenessObservationTransport
}

func (t *failingObservationTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if t.calls.Add(1) == 1 {
		return t.start.RoundTrip(request)
	}
	if request.Body != nil {
		_ = request.Body.Close()
	}
	return &http.Response{StatusCode: http.StatusServiceUnavailable, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"error":{"code":"unavailable","message":"unavailable"}}`)), Request: request}, nil
}

// Every machine sent its presence every 10 s, also while the server failed them, so a server outage
// met ~1,000 requests/s from 10k machines. Failures back off; a healthy server is still reported to
// at about the interval.
func TestRuntimeObservationBacksOffWhileTheServerFails(t *testing.T) {
	run := func(transport http.RoundTripper) {
		sender := &runtimeObservationSender{
			endpoint: "https://observations.invalid/v1/runtime-observations", tokens: livenessObservationTokenSource{}, proofs: livenessObservationProofSource{},
			operationID: func() (string, error) { return "op_runtime_observation_backoff_0001", nil }, environmentID: "env_runtime_observation_backoff",
			machineID: "machine_runtime_observation_backoff", reporterVersion: "test", client: &http.Client{Transport: transport},
			receiptPath: filepath.Join(t.TempDir(), "runtime", "server-heartbeat.json"), workerGeneration: 1, osBootID: "boot-runtime-observation-backoff",
		}
		service := &runtimeObservationService{sender: sender, interval: 50 * time.Millisecond, timeout: time.Second}
		if err := service.Start(t.Context()); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2500 * time.Millisecond)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = service.Shutdown(ctx) // reports the last failed observation, expected here
	}
	failing := &failingObservationTransport{start: livenessObservationTransport{notify: make(chan struct{}, 4)}}
	run(failing)
	healthy := &livenessObservationTransport{notify: make(chan struct{}, 1024)}
	run(healthy)
	// Failing: about 50, 50, 100, 200, 400, 800 ms waits (~8 sends in 2.5 s); healthy: one per ~50 ms.
	t.Logf("observations in 2.5 s at a 50 ms interval: failing server %d, healthy server %d", failing.calls.Load(), healthy.calls.Load())
	if failing.calls.Load() > 12 || failing.calls.Load()*2 > healthy.calls.Load() {
		t.Fatalf("a failing server got %d observations against %d for a healthy one; want a backoff", failing.calls.Load(), healthy.calls.Load())
	}
}
