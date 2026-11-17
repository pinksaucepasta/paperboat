package api

import (
	"context"
	"errors"
	"github.com/pinksaucepasta/paperboat/shared/serverclock"
	"net/http"
	"net/url"

	"github.com/pinksaucepasta/paperboat/shared/inspector"
)

type InspectorCarrier = inspector.Carrier
type InspectorAccess = inspector.Access

func (c *Client) ResolveInspectorAccess(ctx context.Context, token, kind, resource, route, action, transport string) (InspectorAccess, error) {
	var out InspectorAccess
	err := c.doChecked(ctx, http.MethodPost, "/v1/inspector/access", map[string]string{"credential_token": token, "resource_kind": kind, "resource_id": resource, "route_id": route, "action": action, "transport": transport}, &out)
	if err != nil {
		return InspectorAccess{}, err
	}
	if out.CredentialID == "" || out.MachineID == "" || out.MachineGeneration == 0 || out.ResourceKind != kind || out.ResourceID != resource || out.RouteID != route || !out.ExpiresAt.After(serverclock.Now().UTC()) {
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

type InspectorTarget = inspector.Target

func (c *Client) ResolveInspectorTargets(ctx context.Context, selector, action string) ([]InspectorTarget, error) {
	var out []InspectorTarget
	if err := c.doChecked(ctx, http.MethodPost, "/v1/inspector/targets", map[string]string{"selector": selector, "action": action}, &out); err != nil {
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
