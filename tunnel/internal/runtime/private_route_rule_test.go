package runtime

import (
	"testing"

	"github.com/pinksaucepasta/paperboat/tunnel/internal/control"
	"github.com/pinksaucepasta/paperboat/tunnel/internal/route"
)

// pbvm3 2026-10-26: a private TCP route on an engine tunnel never became ready because its probe
// rule had no route generation, so the edge's exact-assignment check (binding RouteGeneration =
// route revision) refused it. The rule carries the route id and generation like the other kinds.
func TestPrivateRouteRuleCarriesTheRouteGenerationItsBindingChecks(t *testing.T) {
	rules := canonicalPrivateRouteRules([]control.RouteAssignment{{RouteID: "route_ptcp", Revision: 4, Kind: string(route.TunnelPrivateTCP)}})
	if len(rules) != 1 || rules[0].RouteGeneration != 4 || rules[0].RouteID != "route_ptcp" {
		t.Fatalf("private route rule = %+v, want route id route_ptcp and generation 4", rules)
	}
}
