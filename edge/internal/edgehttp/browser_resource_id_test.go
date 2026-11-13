package edgehttp

import (
	"testing"

	"github.com/pinksaucepasta/paperboat/edge/internal/route"
)

// 2026-11-05: a private tunnel riding its machine's runtime carrier answered its own owner 403: the edge
// sent the carrier's route id as the resource and the server refused the session (selector_mismatch).
func TestBrowserAuthorizeUsesTheTunnelNotItsCarrier(t *testing.T) {
	if got := browserResourceID(route.RouteRule{TunnelID: "route_runtime_carrier", ResourceID: "tunnel_owner"}); got != "tunnel_owner" {
		t.Fatalf("runtime-carried tunnel resource = %q, want the tunnel", got)
	}
	if got := browserResourceID(route.RouteRule{TunnelID: "preview_publication"}); got != "preview_publication" {
		t.Fatalf("preview resource = %q, want its publication", got)
	}
}
