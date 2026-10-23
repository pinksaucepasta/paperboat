package tunnelmanager

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// PLAN 3a step 4.3: a tunnel served on the agent's runtime carriers runs no connector. A running
// connector is drained when its tunnel moves, without a stopped observation (the engine reports the
// tunnel's health), and the connector comes back when the tunnel leaves the engine.
func TestManagerRunsNoConnectorForAnEngineServedTunnel(t *testing.T) {
	now := time.Date(2026, 10, 23, 14, 0, 0, 0, time.UTC)
	store := &memoryStateStore{state: tunnelState(t, 1, 0), revision: 1}
	first, second := &fakeActive{}, &fakeActive{}
	factory := &fakeFactory{candidates: map[uint64][]*fakeCandidate{
		1: {{probe: ProbeResult{Ready: true, HealthyRoutes: []string{"route_01"}}, active: first}, {probe: ProbeResult{Ready: true, HealthyRoutes: []string{"route_01"}}, active: second}},
	}, err: map[uint64]error{}}
	var observations []Observation
	var engine atomic.Bool
	manager, err := New(Config{Store: store, Factory: factory, HostID: "host_01", Clock: func() time.Time { return now }, ReconcileInterval: time.Hour, ApplyTimeout: time.Second, DrainTimeout: time.Second,
		Report: func(o Observation) { observations = append(observations, o) }, EngineServed: func(id string) bool { return id == "tunnel_01" && engine.Load() }})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer manager.Shutdown(context.Background())
	if got := manager.ResourceCounts()["tunnels"]; got != 1 || len(factory.requests) != 1 {
		t.Fatalf("connector before the move: tunnels=%d applies=%d", got, len(factory.requests))
	}
	before := len(observations)
	engine.Store(true)
	if err := manager.ReconcileNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := manager.ResourceCounts()["tunnels"]; got != 0 || first.drains != 1 || first.closes != 1 {
		t.Fatalf("engine tunnel kept its connector: tunnels=%d drains=%d closes=%d", got, first.drains, first.closes)
	}
	for _, o := range observations[before:] {
		if o.Code == CodeStopped {
			t.Fatalf("moved tunnel reported stopped: %+v", o)
		}
	}
	if err := manager.ReconcileNow(context.Background()); err != nil || len(factory.requests) != 1 {
		t.Fatalf("engine tunnel applied a connector again: %v applies=%d", err, len(factory.requests))
	}
	engine.Store(false)
	if err := manager.ReconcileNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := manager.ResourceCounts()["tunnels"]; got != 1 || len(factory.requests) != 2 {
		t.Fatalf("connector not back after leaving the engine: tunnels=%d applies=%d", got, len(factory.requests))
	}
}
