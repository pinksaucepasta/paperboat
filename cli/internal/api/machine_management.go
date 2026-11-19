package api

import (
	"context"
	"net/http"
	"net/url"
)

// MachineUpdateStatus is the server's stored update observation of one machine (state, current and target version).
func (c *Client) MachineUpdateStatus(ctx context.Context, id string) (out map[string]any, err error) {
	err = c.do(ctx, http.MethodGet, "/v1/machines/"+url.PathEscape(id)+"/update-status", nil, &out)
	return
}
