package edgehttp

import (
	"testing"

	"github.com/pinksaucepasta/paperboat/edge/internal/datacarrier"
	"github.com/pinksaucepasta/paperboat/shared/connectorprotocol"
)

// An endpoint-route preview has lease generation 0 on the edge (its admission carries none) while the
// server's preview_routes view and hostd bind it as generation 1. The grant request hostd presents on the
// owner's loopback proxy must still match the edge's rule, or the request falls through to browser sign-in
// (macOS standard user: CONNECT 200, then HTTP 401 inside the tunnel, 2026-11-05).
func TestEndpointPreviewRuleMatchesTheOwnersPrivateAccessRequest(t *testing.T) {
	preview := DataCarrierPreviewRoute{
		RouteID: "preview_1", PreviewID: "preview_1", OperationID: "preview_1", Hostname: "p.example.test", Kind: dataCarrierPreviewPrivateRouteKind, AccessMode: "private",
		Revision: 1, EndpointRoute: true, AttachmentGeneration: 7, EdgeNodeID: "edge_1", EdgeProcessEpoch: "epoch_1",
		Identity: datacarrier.Identity{AccountID: "account_1", HostID: "machine_1", ConnectorID: "connector_1", SessionID: "session_1", ProcessGeneration: 2, Generation: 3},
	}
	match := previewRouteMatch(preview)
	match.Host = preview.Hostname
	request := connectorprotocol.PrivateAccessRequest{
		AccountID: "account_1", ResourceKind: "preview", ResourceID: "preview_1", RouteID: "preview_1", OperationID: "preview_1",
		Host: "p.example.test", CarrierSessionID: "session_1", RouteGeneration: 1, ProcessGeneration: 2, ConfigGeneration: 3,
		SessionGeneration: 1, AssignmentGeneration: 1, EdgeNodeID: "edge_1", EdgeProcessEpoch: "epoch_1",
	}
	if !privateAccessRouteMatches(request, match) {
		t.Fatalf("the edge rule %+v does not match the owner's request %+v", match.Rule, request)
	}
}
