package tunnelmanager

import (
	"context"
	"sort"
	"testing"
	"time"
)

func purgedRoutes(routes ...string) *fakeActive {
	return &fakeActive{tunnelID: "tunnel_01", connectorID: "connector_01", generation: 1, hash: "hash-inspector", routes: routes}
}

func (a *fakeActive) activeRouteIDs() []string {
	if a == nil {
		return nil
	}
	return append([]string(nil), a.routes...)
}

// TestManagerPurgesRemovedInspectorRoutes proves durable lifecycle owns
// capture cleanup: promoting a generation without a route purges exactly that
// route's retained captures (denying retrieval/replay immediately), while
// untouched routes keep their history.
func TestManagerPurgesRemovedInspectorRoutes(t *testing.T) {
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	state := tunnelState(t, 2, 1)
	store := &memoryStateStore{state: state, revision: 7}
	lkgActive := purgedRoutes("route_01", "route_02")
	lkgActive.generation = 1
	desiredActive := purgedRoutes("route_01")
	desiredActive.generation = 2
	factory := &fakeFactory{candidates: map[uint64][]*fakeCandidate{
		1: {{probe: ProbeResult{Ready: true}, active: lkgActive}},
		2: {{probe: ProbeResult{Ready: true}, active: desiredActive}},
	}, err: map[uint64]error{}}
	var purged [][]string
	manager := newTestManager(t, store, factory, now, func(Observation) {})
	manager.config.InspectorPurge = func(routeIDs []string) {
		purged = append(purged, append([]string(nil), routeIDs...))
	}
	if err := manager.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(purged) != 1 {
		t.Fatalf("purge calls = %v, want exactly one removal", purged)
	}
	got := append([]string(nil), purged[0]...)
	sort.Strings(got)
	if len(got) != 1 || got[0] != "route_02" {
		t.Fatalf("purged routes = %v, want [route_02]", got)
	}
	if err := manager.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// TestManagerPurgesAllRoutesOnRemoveAndDrain proves pause/deletion purges
// every route of the removed active generation immediately.
func TestManagerPurgesAllRoutesOnRemoveAndDrain(t *testing.T) {
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	store := &memoryStateStore{state: tunnelState(t, 1, 1), revision: 1}
	active := purgedRoutes("route_01", "route_02")
	factory := &fakeFactory{candidates: map[uint64][]*fakeCandidate{
		1: {{probe: ProbeResult{Ready: true}, active: active}},
	}, err: map[uint64]error{}}
	var purged [][]string
	manager := newTestManager(t, store, factory, now, func(Observation) {})
	manager.config.InspectorPurge = func(routeIDs []string) {
		purged = append(purged, append([]string(nil), routeIDs...))
	}
	if err := manager.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(purged) != 0 {
		t.Fatalf("initial promotion must not purge: %v", purged)
	}
	manager.removeAndDrain(context.Background(), "tunnel_01", active)
	if len(purged) != 1 || len(purged[0]) != 2 {
		t.Fatalf("removal purge = %v, want both routes", purged)
	}
	if err := manager.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}
