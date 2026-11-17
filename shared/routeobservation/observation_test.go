package routeobservation

import (
	"encoding/json"
	"testing"
)

func TestRouteObservationWire(t *testing.T) {
	raw, err := json.Marshal(RouteObservation{RouteID: "r", RouteRevision: 2, EdgeNodeID: "n", ConnectorGeneration: 3, State: "ready"})
	want := `{"route_id":"r","route_revision":2,"edge_node_id":"n","connector_generation":3,"state":"ready"}`
	if err != nil || string(raw) != want {
		t.Fatalf("observation = %s, %v", raw, err)
	}
}
