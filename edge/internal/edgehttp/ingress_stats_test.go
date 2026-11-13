package edgehttp

import (
	"testing"
	"time"
)

// A route served by two carriers is one attached route: diagnostics compares this count with the
// distinct routes in the registry, so summing per carrier made /readyz report route_drift forever.
func TestIngressStatsCountsDistinctRoutesAcrossCarriers(t *testing.T) {
	registry, err := NewDataCarrierRouteRegistry(DataCarrierRouteRegistryConfig{MaximumRoutes: 8})
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	rule := replicaRule()
	for _, name := range []string{"a", "b"} {
		server, client := testEdgePreviewCarrierPair(t, replicaIdentity("host_"+name, "connector_"+name, "session_"+name, 3))
		defer server.Close()
		defer client.Close()
		if err := registry.AttachReplica(server, "key-"+name, "thumb-"+name, replicaState(rule, 3, "assignment_"+name, "zone_"+name, time.Millisecond, 2)); err != nil {
			t.Fatal(err)
		}
	}
	if stats := registry.Stats(); stats.Sessions != 2 || stats.Routes != 1 {
		t.Fatalf("stats=%+v, want 2 sessions and 1 distinct route", stats)
	}
}
