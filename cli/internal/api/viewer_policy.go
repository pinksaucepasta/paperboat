package api

import (
	"context"
	"net/http"
	"net/url"
)

// ViewerAudienceSpecificEmails is the audience of an allow list: exact addresses and "*@domain" entries.
const ViewerAudienceSpecificEmails = "specific_emails"

// ViewerPolicy is who may open one preview or tunnel route in a browser.
type ViewerPolicy struct {
	Audience      string   `json:"audience,omitempty"`
	AllowedEmails []string `json:"allowed_emails,omitempty"`
	Generation    int64    `json:"generation"`
	Configured    bool     `json:"configured"`
}

// ViewerPolicyInput replaces a route's whole policy; ExpectedGeneration is 0 for the first save.
type ViewerPolicyInput struct {
	Audience           string   `json:"audience"`
	AllowedEmails      []string `json:"allowed_emails,omitempty"`
	ExpectedGeneration int64    `json:"expected_generation"`
}

func viewerPolicyPath(kind, resourceID, routeID string) string {
	return "/v1/browser-access/policies/" + url.PathEscape(kind) + "/" + url.PathEscape(resourceID) + "/" + url.PathEscape(routeID)
}

func (c *Client) GetViewerPolicy(ctx context.Context, kind, resourceID, routeID string) (ViewerPolicy, error) {
	var out ViewerPolicy
	err := c.do(ctx, http.MethodGet, viewerPolicyPath(kind, resourceID, routeID), nil, &out)
	return out, err
}

func (c *Client) SetViewerPolicy(ctx context.Context, kind, resourceID, routeID string, input ViewerPolicyInput) (ViewerPolicy, error) {
	var out ViewerPolicy
	err := c.do(ctx, http.MethodPut, viewerPolicyPath(kind, resourceID, routeID), input, &out)
	return out, err
}
