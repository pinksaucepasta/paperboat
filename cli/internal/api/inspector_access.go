package api

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"time"
)

type InspectorCarrier struct {
	LeaseGeneration      uint64 `json:"lease_generation"`
	AttachmentGeneration uint64 `json:"attachment_generation"`
	OwnerDeviceID        string `json:"owner_device_id"`
	MachineID            string `json:"machine_id"`
	RouteGeneration      uint64 `json:"route_generation"`
	AccountID            string `json:"account_id"`
	TunnelID             string `json:"tunnel_id"`
	ConnectorID          string `json:"connector_id"`
	SessionID            string `json:"session_id"`
	ProcessGeneration    uint64 `json:"process_generation"`
	Generation           uint64 `json:"generation"`
	RouteID              string `json:"route_id"`
	EdgeNodeID           string `json:"edge_node_id"`
	EdgeProcessEpoch     string `json:"edge_process_epoch"`
	AssignmentID         string `json:"assignment_id"`
	AssignmentGeneration uint64 `json:"assignment_generation"`
	ConfigContentHash    string `json:"config_content_hash"`
}
type InspectorAccess struct {
	CredentialID       string           `json:"credential_id"`
	OwnerAccountID     string           `json:"owner_account_id"`
	MachineID          string           `json:"machine_id"`
	MachineGeneration  uint64           `json:"machine_generation"`
	ResourceKind       string           `json:"resource_kind"`
	ResourceID         string           `json:"resource_id"`
	RouteID            string           `json:"route_id"`
	ResourceGeneration uint64           `json:"resource_generation"`
	RouteGeneration    uint64           `json:"route_generation"`
	TargetGeneration   uint64           `json:"target_generation"`
	ExpiresAt          time.Time        `json:"expires_at"`
	EdgeURL            string           `json:"edge_url"`
	Carrier            InspectorCarrier `json:"carrier"`
}

func (c *Client) ResolveInspectorAccess(ctx context.Context, token, kind, resource, route, action, transport string) (InspectorAccess, error) {
	var out InspectorAccess
	err := c.doStrict(ctx, http.MethodPost, "/v1/inspector/access", map[string]string{"credential_token": token, "resource_kind": kind, "resource_id": resource, "route_id": route, "action": action, "transport": transport}, &out)
	if err != nil {
		return InspectorAccess{}, err
	}
	if out.CredentialID == "" || out.MachineID == "" || out.MachineGeneration == 0 || out.ResourceKind != kind || out.ResourceID != resource || out.RouteID != route || !out.ExpiresAt.After(time.Now().UTC()) {
		return InspectorAccess{}, errors.New("unsafe inspector access route")
	}
	return out, nil
}

func (c *Client) RevokeInspectorCredential(ctx context.Context, id string) error {
	if id == "" {
		return errors.New("inspector credential ID is required")
	}
	return c.do(ctx, http.MethodDelete, "/v1/inspector/credentials/"+url.PathEscape(id), nil, nil)
}

type InspectorTarget struct {
	ResourceID string `json:"resource_id"`
	RouteID    string `json:"route_id"`
	RouteName  string `json:"route_name"`
}

func (c *Client) ResolveInspectorTargets(ctx context.Context, selector, action string) ([]InspectorTarget, error) {
	var out []InspectorTarget
	if err := c.doStrict(ctx, http.MethodPost, "/v1/inspector/targets", map[string]string{"selector": selector, "action": action}, &out); err != nil {
		return nil, err
	}
	if len(out) == 0 || len(out) > 128 {
		return nil, errors.New("unsafe inspector target list")
	}
	for _, target := range out {
		if target.ResourceID == "" || target.RouteID == "" {
			return nil, errors.New("unsafe inspector target")
		}
	}
	return out, nil
}
