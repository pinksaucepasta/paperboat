//go:build darwin || linux || windows

package runtime

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/machineservices"
	"github.com/pinksaucepasta/paperboat/shared/serverclock"
)

// A machine whose clock is 10 minutes fast or slow (X-27) reports sampled_at and the process start on the
// server's clock: the server refused the whole heartbeat (400, machine offline) when they were ahead of its time.
func TestObservationTimesAreOnTheServerClockWhenTheMachineClockIsOff(t *testing.T) {
	for _, skew := range []time.Duration{10 * time.Minute, -10 * time.Minute} {
		serverNow := time.Now().Add(-skew) // the machine's clock reads skew ahead of this
		sent := time.Now()
		serverclock.Observe(serverNow.UTC().Format(http.TimeFormat), sent, sent.Add(50*time.Millisecond))
		var body struct {
			SampledAt       time.Time `json:"sampled_at"`
			MachineServices *struct {
				StartedAt time.Time `json:"started_at"`
			} `json:"machine_services"`
			LazyRuntime *struct {
				StartedAt time.Time `json:"started_at"`
			} `json:"lazy_runtime"`
		}
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewDecoder(r.Body).Decode(&body)
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{}})
		}))
		s := &runtimeObservationSender{endpoint: server.URL, machineID: "machine_a", environmentID: "env_a", reporterVersion: "dev", installationGeneration: 7, lazyBootID: "boot_a", lazyStartedAt: time.Now().Add(-time.Minute).UTC(), machineServicesGeneration: 1,
			tokens: livenessObservationTokenSource{}, proofs: livenessObservationProofSource{}, operationID: func() (string, error) { return "operation_a", nil }, client: server.Client(),
			machineServicesDiscover: func(context.Context) ([]machineservices.Service, error) { return nil, nil }}
		err := s.Send(context.Background())
		server.Close()
		serverclock.Observe(time.Now().UTC().Format(http.TimeFormat), time.Now(), time.Now())
		if err != nil {
			t.Fatalf("skew %v: %v", skew, err)
		}
		if body.SampledAt.IsZero() || body.SampledAt.Sub(serverNow).Abs() > 5*time.Second {
			t.Fatalf("skew %v: sampled_at %v is not the server's time %v", skew, body.SampledAt, serverNow)
		}
		for name, started := range map[string]time.Time{"machine_services": startedAt(body.MachineServices), "lazy_runtime": startedAt(body.LazyRuntime)} {
			if started.IsZero() || started.After(serverNow.Add(5*time.Second)) {
				t.Fatalf("skew %v: %s started_at %v is ahead of the server's time %v", skew, name, started, serverNow)
			}
		}
	}
}

func startedAt(v *struct {
	StartedAt time.Time `json:"started_at"`
}) time.Time {
	if v == nil {
		return time.Time{}
	}
	return v.StartedAt
}
