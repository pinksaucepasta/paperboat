package control

import (
	"context"
	"errors"
	"time"

	"github.com/pinksaucepasta/paperboat/shared/connectorprotocol"
	"github.com/pinksaucepasta/paperboat/shared/inspector"
)

const InspectorAccessPath = "/v1/edge/inspector/access"

var (
	ErrInspectorAccessInvalid     = errors.New("invalid inspector edge access request")
	ErrInspectorAccessUnavailable = errors.New("inspector edge access unavailable")
	// ErrInspectorAccessDenied: the server answered 403 inspector_not_authorized.
	ErrInspectorAccessDenied = errors.New("inspector edge access denied")
)

type InspectorAccessRequest struct {
	EdgeNodeID      string `json:"edge_node_id"`
	ProcessEpoch    string `json:"process_epoch"`
	CredentialToken string `json:"credential_token"`
	ResourceKind    string `json:"resource_kind"`
	ResourceID      string `json:"resource_id"`
	RouteID         string `json:"route_id"`
	Action          string `json:"action"`
}

type InspectorCarrier = inspector.Carrier

type InspectorAccess = inspector.Access

type InspectorAccessClient struct {
	HTTP         *HTTPClient
	NodeID       string
	ProcessEpoch string
}

func (c *InspectorAccessClient) Authorize(ctx context.Context, token, kind, resource, route, action string) (InspectorAccess, error) {
	if c == nil || c.HTTP == nil || ctx == nil || connectorprotocol.ValidateIdentifier(c.NodeID) != nil || connectorprotocol.ValidateOpaqueEpoch(c.ProcessEpoch) != nil || token == "" || len(token) > 512 || (kind != "preview" && kind != "tunnel") || connectorprotocol.ValidateIdentifier(resource) != nil || connectorprotocol.ValidateIdentifier(route) != nil || (action != "inspect" && action != "replay") {
		return InspectorAccess{}, ErrInspectorAccessInvalid
	}
	in := InspectorAccessRequest{EdgeNodeID: c.NodeID, ProcessEpoch: c.ProcessEpoch, CredentialToken: token, ResourceKind: kind, ResourceID: resource, RouteID: route, Action: action}
	var out InspectorAccess
	if err := c.HTTP.postNodeWithMaximumAndIdentity(ctx, InspectorAccessPath, c.NodeID, c.ProcessEpoch, in, &out, 64<<10); err != nil {
		return InspectorAccess{}, errors.Join(ErrInspectorAccessUnavailable, err)
	}
	if out.CredentialID == "" || out.OwnerAccountID == "" || out.MachineID == "" || out.ResourceKind != kind || out.ResourceID != resource || out.RouteID != route || out.ResourceGeneration == 0 || out.RouteGeneration == 0 || out.TargetGeneration == 0 || !out.ExpiresAt.After(time.Now().UTC()) || out.Carrier.AccountID != out.OwnerAccountID || out.Carrier.MachineID != out.MachineID || out.Carrier.EdgeNodeID != c.NodeID || out.Carrier.EdgeProcessEpoch != c.ProcessEpoch || out.Carrier.TunnelID == "" || out.Carrier.ConnectorID == "" || out.Carrier.SessionID == "" || out.Carrier.RouteID == "" || out.Carrier.ProcessGeneration == 0 || out.Carrier.Generation == 0 || out.Carrier.AssignmentID == "" || out.Carrier.AssignmentGeneration == 0 || out.Carrier.RouteGeneration == 0 || out.Carrier.ConfigContentHash == "" {
		return InspectorAccess{}, ErrInspectorAccessInvalid
	}
	return out, nil
}
