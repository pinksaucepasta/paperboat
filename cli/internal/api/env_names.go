package api

import (
	"context"
	"net/http"
)

// ENVNamesListable is the account's personal-workspace setting "Commands on machines can list ENV names"
// (GET /v1/me/preferences).
func (c *Client) ENVNamesListable(ctx context.Context) (bool, error) {
	var out struct {
		ENVNamesListable bool `json:"env_names_listable"`
	}
	err := c.do(ctx, http.MethodGet, "/v1/me/preferences", nil, &out)
	return out.ENVNamesListable, err
}

// SetENVNamesListable changes only that setting (PATCH /v1/me/preferences).
func (c *Client) SetENVNamesListable(ctx context.Context, on bool) error {
	return c.do(ctx, http.MethodPatch, "/v1/me/preferences", map[string]bool{"env_names_listable": on}, &struct{}{})
}
