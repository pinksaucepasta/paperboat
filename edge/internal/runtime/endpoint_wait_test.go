package runtime

import (
	"context"
	"sync"
	"testing"
	"time"

	runtimewire "github.com/pinksaucepasta/paperboat/shared/runtimeattachment"

	"github.com/pinksaucepasta/paperboat/edge/internal/control"
	"github.com/pinksaucepasta/paperboat/edge/internal/datacarrier"
)

// heldSource is a control plane that holds delta endpoint pulls like the server does: a pull with a cursor
// blocks until a change is injected, the wait passes or the request is cancelled.
type heldSource struct {
	*runtimeSourceFake
	inject    chan control.EndpointUpdate
	failDelta bool
	pullMu    sync.Mutex
	deltas    int
	waits     []time.Duration
}

func (s *heldSource) EndpointAdmissions(ctx context.Context, _, _, since string, wait time.Duration) (control.EndpointUpdate, error) {
	if since == "" {
		return control.EndpointUpdate{Complete: true, Admissions: []runtimewire.Admission{}, Cursor: "c"}, nil
	}
	s.pullMu.Lock()
	s.deltas++
	s.waits = append(s.waits, wait)
	failing := s.failDelta
	s.pullMu.Unlock()
	if failing {
		return control.EndpointUpdate{}, control.ErrControlUnavailable
	}
	idle := control.EndpointUpdate{Admissions: []runtimewire.Admission{}, Cursor: "c", ExpiresAt: time.Now().Add(time.Hour)}
	select {
	case update := <-s.inject:
		return update, nil
	case <-time.After(wait):
		return idle, nil
	case <-ctx.Done():
		return control.EndpointUpdate{}, ctx.Err()
	}
}

func (s *heldSource) deltaPulls() int {
	s.pullMu.Lock()
	defer s.pullMu.Unlock()
	return s.deltas
}

func startHeldWorker(t *testing.T, source *heldSource, wait time.Duration) (*RuntimeCarrierWorker, *datacarrier.ExpectedAdmissionRegistry) {
	t.Helper()
	runtimeRoute := runtimeAdmission(t)
	epoch := runtimeRoute.Binding.EdgeProcessEpoch
	expected, err := datacarrier.NewExpectedAdmissionRegistry(datacarrier.ExpectedAdmissionRegistryConfig{NodeID: runtimeRoute.Binding.EdgeNodeID, ProcessEpoch: epoch})
	if err != nil {
		t.Fatal(err)
	}
	w := &RuntimeCarrierWorker{Source: source, Expected: expected, Routes: endpointRegistry(t, epoch), Endpoints: endpointRegistry(t, epoch),
		Interval: time.Minute, EndpointInterval: 50 * time.Millisecond, EndpointWait: wait, Timeout: time.Second}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := w.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Shutdown(context.Background()) })
	return w, expected
}

// A held endpoint pull is answered the moment the server has a change: the worker installs it within 150 ms
// (the old 500 ms poll took 250 ms on average), and while idle it makes about one request per wait, not one
// per EndpointInterval.
func TestEndpointPullHeldAtTheServerInstallsAChangeAtOnce(t *testing.T) {
	runtimeRoute := runtimeAdmission(t)
	source := &heldSource{runtimeSourceFake: &runtimeSourceFake{admissions: []runtimewire.Admission{runtimeRoute}}, inject: make(chan control.EndpointUpdate)}
	const wait = 500 * time.Millisecond
	_, expected := startHeldWorker(t, source, wait)
	waitRuntime(t, func() bool { return len(expected.Snapshot()) == 1 && source.deltaPulls() >= 1 })
	before, started := source.deltaPulls(), time.Now()
	time.Sleep(2 * time.Second)
	idle := source.deltaPulls() - before
	t.Logf("idle: %d endpoint requests in %v with a %v wait (the 50 ms interval allowed %d)", idle, time.Since(started).Round(time.Millisecond), wait, int(2*time.Second/(50*time.Millisecond)))
	if idle > 5 { // 2 s / 500 ms = 4, plus one that straddles the window
		t.Fatalf("%d endpoint requests in 2 s while idle, want about one per %v", idle, wait)
	}
	added := endpointAdmission(runtimeRoute, "public")
	delta := control.EndpointUpdate{Admissions: []runtimewire.Admission{added}, Cursor: "c2", ExpiresAt: time.Now().Add(time.Hour)}
	injected := time.Now()
	source.inject <- delta
	waitRuntime(t, func() bool { return len(expected.Snapshot()) == 2 })
	if took := time.Since(injected); took > 150*time.Millisecond {
		t.Fatalf("change installed after %v, want within 150 ms", took)
	}
	source.pullMu.Lock()
	defer source.pullMu.Unlock()
	for _, w := range source.waits {
		if w != wait {
			t.Fatalf("delta pull asked the server to hold for %v, want %v", w, wait)
		}
	}
}

// After a failed endpoint pull the worker pauses endpoint pulls until the next runtime pull instead of retrying
// at EndpointInterval: a failing server sees one request, not twenty a second.
func TestEndpointPullDoesNotHammerAFailingServer(t *testing.T) {
	runtimeRoute := runtimeAdmission(t)
	source := &heldSource{runtimeSourceFake: &runtimeSourceFake{admissions: []runtimewire.Admission{runtimeRoute}}, inject: make(chan control.EndpointUpdate), failDelta: true}
	startHeldWorker(t, source, time.Second)
	time.Sleep(1500 * time.Millisecond)
	if n := source.deltaPulls(); n != 1 {
		t.Fatalf("%d endpoint requests in 1.5 s against a failing server, want 1", n)
	}
}
