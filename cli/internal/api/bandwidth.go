package api

import (
	"context"
	"net/http"
	"net/url"
	"time"
)

// WorkspaceBandwidth is the fair-use state reported by Billing.
type WorkspaceBandwidth struct {
	Throttled    bool      `json:"throttled"`
	SlowMbit     float64   `json:"throttle_mbit"`
	AllowanceEnd time.Time `json:"allowance_period_end"`
}

func (c *Client) WorkspaceBandwidth(ctx context.Context, teamID string) (WorkspaceBandwidth, error) {
	path := "/v1/billing/workspaces/personal"
	if teamID != "" {
		path = "/v1/billing/workspaces/teams/" + url.PathEscape(teamID)
	}
	var bandwidth WorkspaceBandwidth
	err := c.do(ctx, http.MethodGet, path, nil, &bandwidth)
	return bandwidth, err
}
