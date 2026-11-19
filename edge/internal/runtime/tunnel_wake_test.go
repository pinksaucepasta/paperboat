package runtime

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	runtimewire "github.com/pinksaucepasta/paperboat/shared/runtimeattachment"

	"github.com/pinksaucepasta/paperboat/edge/internal/control"
	"github.com/pinksaucepasta/paperboat/edge/internal/datacarrier"
	"github.com/pinksaucepasta/paperboat/edge/internal/node"
	"github.com/pinksaucepasta/paperboat/edge/internal/route"
)

type countingRouteSource struct{ pulls atomic.Int32 }

func (s *countingRouteSource) DesiredRouteSnapshot(context.Context, string, string) (control.RouteSnapshot, error) {
	s.pulls.Add(1)
	return control.RouteSnapshot{Complete: true}, nil
}

func (s *countingRouteSource) DesiredRoutes(context.Context, string) ([]control.RouteAssignment, error) {
	s.pulls.Add(1)
	return nil, nil
}

// A tunnel assignment change seen by the fast endpoint pull reconciles the RouteWorker at once, not
// at its control interval, once per new change however many pulls repeat it.
func TestTunnelChangeSeenByEndpointPullWakesRouteWorker(t *testing.T) {
	runtimeRoute := runtimeAdmission(t)
	var mu sync.Mutex
	changedAt := ""
	endpoint := &runtimeSourceFake{admissions: []runtimewire.Admission{runtimeRoute}}
	var deltas atomic.Int32 // endpoint pulls that carried a since cursor: the "many empty deltas" the test counts
	endpoint.endpointUpdate = func(since string) control.EndpointUpdate {
		if since != "" {
			deltas.Add(1)
		}
		if since == "" {
			return control.EndpointUpdate{Complete: true, Admissions: []runtimewire.Admission{}, Cursor: "c"}
		}
		mu.Lock()
		defer mu.Unlock()
		return control.EndpointUpdate{Admissions: []runtimewire.Admission{}, Cursor: "c", ExpiresAt: time.Now().Add(time.Hour), TunnelsChangedAt: changedAt}
	}
	epoch := runtimeRoute.Binding.EdgeProcessEpoch
	expected, err := datacarrier.NewExpectedAdmissionRegistry(datacarrier.ExpectedAdmissionRegistryConfig{NodeID: runtimeRoute.Binding.EdgeNodeID, ProcessEpoch: epoch})
	if err != nil {
		t.Fatal(err)
	}
	wake := make(chan struct{}, 1)
	var changes atomic.Int32
	carrier := &RuntimeCarrierWorker{Source: endpoint, Expected: expected, Routes: endpointRegistry(t, epoch), Endpoints: endpointRegistry(t, epoch),
		Interval: time.Minute, EndpointInterval: 5 * time.Millisecond, Timeout: time.Second,
		OnTunnelsChanged: func() {
			changes.Add(1)
			select {
			case wake <- struct{}{}:
			default:
			}
		}}
	routes := &countingRouteSource{}
	registry := route.NewRegistry("", "")
	t.Cleanup(func() { _ = registry.Close() })
	state := node.New("edge_1")
	state.MarkReady()
	worker := &RouteWorker{Registry: registry, Source: routes, State: state, NodeID: "edge_1", ProcessEpoch: "edge_epoch_1",
		TunnelWake: wake, Interval: time.Minute, DrainTimeout: time.Second}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := worker.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = worker.Shutdown(context.Background()) }()
	if err := carrier.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = carrier.Shutdown(context.Background()) }()
	waitRuntime(t, func() bool { return deltas.Load() >= 20 }) // many empty deltas: no wake, no extra route pull
	if c, p := changes.Load(), routes.pulls.Load(); c != 0 || p != 1 {
		t.Fatalf("with no change: %d wakes, %d route pulls, want 0 and the start pull", c, p)
	}
	for i, token := range []string{"2026-11-16T00:00:01Z", "2026-11-16T00:00:02Z"} {
		mu.Lock()
		changedAt = token
		mu.Unlock()
		waitRuntime(t, func() bool { return routes.pulls.Load() == int32(i+2) })
		seen := deltas.Load() // the same token repeats on every delta: still one wake across the next ones
		waitRuntime(t, func() bool { return deltas.Load() >= seen+10 })
		if c := changes.Load(); c != int32(i+1) {
			t.Fatalf("after change %d: %d wakes, want %d", i+1, c, i+1)
		}
	}
}
