package api

import (
	"context"
	"errors"
	"net/http"
	"time"
)

// InspectorCredential is a short-lived opaque credential for one exact
// resource/route/action, issued to the caller's own current authority. The
// token is shown once and must never be logged.
type InspectorCredential struct {
	CredentialID string    `json:"credential_id"`
	Token        string    `json:"token"`
	ExpiresAt    time.Time `json:"expires_at"`
}

// IssueInspectorCredential issues an inspector credential for the caller's
// own account. Issue requires current ownership or an explicit team
// inspect/replay grant; viewing, use/manage grants, membership and login
// alone are denied without side effects.
func (c *Client) IssueInspectorCredential(ctx context.Context, kind, resource, route, action string) (InspectorCredential, error) {
	if ctx == nil {
		return InspectorCredential{}, errors.New("inspector issue requires a context")
	}
	var out InspectorCredential
	if err := c.do(ctx, http.MethodPost, "/v1/inspector/credentials", map[string]any{
		"resource_kind": kind, "resource_id": resource, "route_id": route, "action": action, "ttl_seconds": 300,
	}, &out); err != nil {
		return InspectorCredential{}, err
	}
	if out.Token == "" || out.CredentialID == "" || out.ExpiresAt.IsZero() {
		return InspectorCredential{}, errors.New("paperboat-server returned an unsafe inspector credential")
	}
	return out, nil
}
