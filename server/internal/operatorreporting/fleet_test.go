package operatorreporting

import (
	"errors"
	"github.com/pinksaucepasta/paperboat-server/internal/auth"
	"testing"
)

func TestFleetMetricRejectsUnsafeOrNativeIdentity(t *testing.T) {
	s := &Service{}
	for _, f := range []FleetFilter{{Region: "mumbai"}, {Region: "mumbai", Instance: "slot-65"}, {Region: "mumbai OR component:other", Instance: "slot-01"}} {
		if _, err := s.Metrics(t.Context(), auth.User{}, "paperboat.runtime.goroutines", "paperboat-server", "1h", f); !errors.Is(err, ErrInvalid) {
			t.Fatalf("invalid fleet accepted: %v", err)
		}
	}
	if _, err := s.Metrics(t.Context(), auth.User{}, "paperboat.operation.count", "paperboat-daemon", "1h", FleetFilter{Region: "mumbai", Instance: "slot-01"}); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	e := (sentryEvent{Tags: []sentryTag{{Key: "region", Value: "mumbai"}, {Key: "instance", Value: "slot-01"}}}).safe()
	if e.Region != "mumbai" || e.Instance != "slot-01" {
		t.Fatalf("missing fleet %+v", e)
	}
	e = (sentryEvent{Tags: []sentryTag{{Key: "region", Value: "private.example.com"}, {Key: "instance", Value: "slot-01"}}}).safe()
	if e.Region != "" || e.Instance != "" {
		t.Fatalf("unsafe fleet %+v", e)
	}
}

func TestWorkerMetricFilterRejectsNonEnumAndWrongMetric(t *testing.T) {
	s := &Service{}
	for _, v := range []struct{ metric, worker string }{{"paperboat.worker.state", "private_worker"}, {"paperboat.operation.count", "worker_machines"}} {
		if _, err := s.Metrics(t.Context(), auth.User{}, v.metric, "paperboat-server", "1h", FleetFilter{Worker: v.worker}); !errors.Is(err, ErrInvalid) {
			t.Fatalf("invalid worker filter accepted %v", err)
		}
	}
}
