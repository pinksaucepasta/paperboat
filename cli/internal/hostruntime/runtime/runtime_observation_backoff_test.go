package runtime

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// failingObservationTransport accepts the first observation (the start) and fails every later one, as a
// server that goes down after the machine started. unreachable fails without an answer instead, as the
// machine's own network being down.
type failingObservationTransport struct {
	calls       atomic.Int64
	start       livenessObservationTransport
	unreachable bool
}

func (t *failingObservationTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if t.calls.Add(1) == 1 {
		return t.start.RoundTrip(request)
	}
	if request.Body != nil {
		_ = request.Body.Close()
	}
	if t.unreachable {
		return nil, errors.New("network is unreachable")
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
	// A machine whose network is down keeps the interval: its retries cost the server nothing, and a
	// backoff kept it offline at the server for up to 90 s after the network came back (N3 on pbvm2).
	unreachable := &failingObservationTransport{start: livenessObservationTransport{notify: make(chan struct{}, 4)}, unreachable: true}
	run(unreachable)
	t.Logf("unreachable server %d", unreachable.calls.Load())
	if unreachable.calls.Load() < 2*failing.calls.Load() {
		t.Fatalf("an unreachable server got %d observations against %d for a failing one; want the interval, not the backoff", unreachable.calls.Load(), failing.calls.Load())
	}
	// Failing: about 50, 50, 100, 200, 400, 800 ms waits (~8 sends in 2.5 s); healthy: one per ~50 ms.
	t.Logf("observations in 2.5 s at a 50 ms interval: failing server %d, healthy server %d", failing.calls.Load(), healthy.calls.Load())
	if failing.calls.Load() > 12 || failing.calls.Load()*2 > healthy.calls.Load() {
		t.Fatalf("a failing server got %d observations against %d for a healthy one; want a backoff", failing.calls.Load(), healthy.calls.Load())
	}
}

// After a network outage the server has marked the machine offline and refuses descriptors for it
// (machine_not_ready), so presence is retried within seconds while no connection can be opened. A
// timeout on an open connection may be a hung server and keeps the interval; a server that answered
// with failures backs off.
func TestRuntimeObservationRetriesSoonWhileNoConnectionOpens(t *testing.T) {
	service := &runtimeObservationService{interval: 10 * time.Second}
	dial := runtimeObservationFailure{stage: "control_request", cause: &url.Error{Op: "Post", URL: "https://observations.invalid", Err: &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("i/o timeout")}}}
	dns := runtimeObservationFailure{stage: "control_request", cause: &url.Error{Op: "Post", URL: "https://observations.invalid", Err: &net.OpError{Op: "dial", Net: "tcp", Err: &net.DNSError{Err: "no such host", Name: "observations.invalid"}}}}
	hung := runtimeObservationFailure{stage: "control_request", cause: &url.Error{Op: "Post", URL: "https://observations.invalid", Err: context.DeadlineExceeded}}
	answered := runtimeObservationFailure{stage: "control_request", cause: errors.New("503"), answered: true}
	for range 100 {
		for name, err := range map[string]error{"dial": dial, "dns": dns} {
			if got := service.runtimeObservationWait(err, 0); got >= 3*time.Second {
				t.Fatalf("%s failure waits %v; want under 3 s", name, got)
			}
		}
		if got := service.runtimeObservationWait(hung, 0); got < 5*time.Second {
			t.Fatalf("a timeout on an open connection waits %v; want the interval", got)
		}
		if got := service.runtimeObservationWait(nil, 0); got < 5*time.Second || got >= 15*time.Second {
			t.Fatalf("success waits %v; want the jittered interval", got)
		}
		if got := service.runtimeObservationWait(answered, 4); got < 20*time.Second {
			t.Fatalf("answered failures wait %v; want the backoff", got)
		}
	}
}
