//go:build darwin || linux || windows

package runtime

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/peertransport/networkmonitor"
)

type fakeNetworkObserver struct {
	starts atomic.Int32
	closes atomic.Int32
	err    error
}

type recordedNetworkMetric struct {
	name   string
	value  float64
	labels map[string]string
}

type recordingNetworkMetrics struct {
	mu      sync.Mutex
	records []recordedNetworkMetric
	err     error
}

func (m *recordingNetworkMetrics) Record(name string, value float64, labels map[string]string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	copied := make(map[string]string, len(labels))
	for key, label := range labels {
		copied[key] = label
	}
	m.records = append(m.records, recordedNetworkMetric{name: name, value: value, labels: copied})
	return m.err
}

func (o *fakeNetworkObserver) Start() error { o.starts.Add(1); return o.err }
func (o *fakeNetworkObserver) Close() error { o.closes.Add(1); return o.err }

func TestNetworkChangeServiceOwnsObserverLifecycle(t *testing.T) {
	observer := &fakeNetworkObserver{}
	service := &networkChangeService{observer: observer}
	if err := service.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := service.Start(context.Background()); !errors.Is(err, ErrProductionInvalid) {
		t.Fatalf("second start err=%v", err)
	}
	if err := service.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := service.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if observer.starts.Load() != 1 || observer.closes.Load() != 1 {
		t.Fatalf("starts=%d closes=%d", observer.starts.Load(), observer.closes.Load())
	}
}

func TestNetworkChangeServicePropagatesObserverFailure(t *testing.T) {
	want := errors.New("monitor unavailable")
	service := &networkChangeService{observer: &fakeNetworkObserver{err: want}}
	if err := service.Start(context.Background()); !errors.Is(err, want) {
		t.Fatalf("start err=%v", err)
	}
}

type recordingCanonicalRecovery struct {
	events []networkmonitor.Event
}

func (r *recordingCanonicalRecovery) HandleNetworkEvent(event networkmonitor.Event) {
	r.events = append(r.events, event)
}

func TestNetworkChangeHandlerForwardsCurrentRecoveryEvents(t *testing.T) {
	metrics := &recordingNetworkMetrics{}
	handler, err := newNetworkChangeHandler(metrics)
	if err != nil {
		t.Fatal(err)
	}
	recovery := &recordingCanonicalRecovery{}
	handler.SetCanonical(recovery)
	handler.Handle(networkmonitor.Event{Generation: 7, Rebind: true, Viable: true, Reasons: networkmonitor.ReasonDefaultRoute})
	handler.Handle(networkmonitor.Event{Generation: 8, Reasons: networkmonitor.ReasonDNS})
	handler.Handle(networkmonitor.Event{Reasons: networkmonitor.ReasonWake})
	if len(recovery.events) != 2 || recovery.events[0].Generation != 7 || recovery.events[1].Generation != 8 {
		t.Fatalf("forwarded events=%+v", recovery.events)
	}
	if len(metrics.records) != 4 || metrics.records[1].labels["reason"] != "default_route" || metrics.records[1].labels["action"] != "rebind" || metrics.records[3].labels["reason"] != "dns" || metrics.records[3].labels["action"] != "observe" {
		t.Fatalf("metrics=%+v", metrics.records)
	}
}
