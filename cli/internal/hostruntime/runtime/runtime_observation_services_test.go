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

func quietObservationService(t *testing.T, transport http.RoundTripper, wake <-chan struct{}) (*runtimeObservationService, func()) {
	t.Helper()
	sender := &runtimeObservationSender{
		endpoint:         "https://observations.invalid/v1/runtime-observations",
		tokens:           livenessObservationTokenSource{},
		proofs:           livenessObservationProofSource{},
		operationID:      func() (string, error) { return "op_runtime_observation_quiet_0001", nil },
		environmentID:    "env_quiet",
		machineID:        "machine_quiet",
		reporterVersion:  "test",
		client:           &http.Client{Transport: transport},
		receiptPath:      filepath.Join(t.TempDir(), "runtime", "server-heartbeat.json"),
		workerGeneration: 1,
		osBootID:         "boot-quiet",
		lazyBootID:       "boot-quiet", lazyStartedAt: time.Now(), installationGeneration: 7,
		machineServicesDiscover: func(context.Context) ([]machineservices.Service, error) { return nil, nil },
	}
	service := &runtimeObservationService{sender: sender, interval: time.Hour, heartbeat: time.Hour, wake: wake, timeout: time.Second, servicesWatch: 20 * time.Millisecond}
	if err := service.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	return service, func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = service.Shutdown(shutdownCtx)
	}
}

// An unchanged machine posts one observation (at start), not one per interval: presence is the held machine-events
// wait, and a heartbeat is hours away here. Shutdown sends the last one, which the count below excludes.
func TestRuntimeObservationIsNotRepeatedWhileNothingChanges(t *testing.T) {
	transport := &policyObservationTransport{}
	_, stop := quietObservationService(t, transport, nil)
	defer stop()
	time.Sleep(400 * time.Millisecond)
	if calls := transport.calls.Load(); calls != 1 {
		t.Fatalf("%d observations in 400 ms of an unchanged machine, want 1", calls)
	}
}

// A machine event that moved a policy the response carries sends an observation at once.
func TestRuntimeObservationIsSentAtOnceWhenAPolicyEventWakesIt(t *testing.T) {
	transport := &policyObservationTransport{}
	wake := make(chan struct{}, 1)
	_, stop := quietObservationService(t, transport, wake)
	defer stop()
	time.Sleep(100 * time.Millisecond)
	before := transport.calls.Load()
	wake <- struct{}{}
	deadline := time.Now().Add(2 * time.Second)
	for transport.calls.Load() <= before {
		if time.Now().After(deadline) {
			t.Fatalf("a policy wake did not send an observation (calls=%d)", transport.calls.Load())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestTimelessDigestIgnoresTimestampsOnly(t *testing.T) {
	a := timelessDigest([]byte(`{"sampled_at":"2026-11-16T01:00:00Z","update":{"state":"idle","operation_id":"update-1-111","observed_at":"2026-11-16T01:00:01.5Z"},"machine_services":{"generation":3}}`))
	b := timelessDigest([]byte(`{"sampled_at":"2026-11-16T01:00:10Z","update":{"state":"idle","operation_id":"update-1-222","observed_at":"2026-11-16T01:00:11Z"},"machine_services":{"generation":3}}`))
	c := timelessDigest([]byte(`{"sampled_at":"2026-11-16T01:00:10Z","update":{"state":"failed","observed_at":"2026-11-16T01:00:11Z"},"machine_services":{"generation":3}}`))
	if a != b {
		t.Fatal("two observations that differ only in their times have different digests")
	}
	if a == c {
		t.Fatal("a changed state has the same digest")
	}
}
