package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// HostedEdge is one of Paperboat's regions as the person sees it: the region id and display name with the
// status of its servers folded together. The server never sends a node id, host or heartbeat here.
type HostedEdge struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
}

type HostedEdgePage struct {
	Items      []HostedEdge `json:"items"`
	NextCursor string       `json:"next_cursor,omitempty"`
	ObservedAt time.Time    `json:"observed_at"`
}

// ListHostedEdges lists the regions Paperboat serves tunnels and previews from.
func (c *Client) ListHostedEdges(ctx context.Context) (HostedEdgePage, error) {
	return c.listHostedRegions(ctx, "/v1/edges", "edge")
}

// ListHostedRelays lists the regions Paperboat runs relays in (same shape as the edge regions).
func (c *Client) ListHostedRelays(ctx context.Context) (HostedEdgePage, error) {
	return c.listHostedRegions(ctx, "/v1/relays", "relay")
}

func (c *Client) listHostedRegions(ctx context.Context, route, what string) (HostedEdgePage, error) {
	const pageSize = 100
	invalid := errors.New("paperboat-server returned invalid " + what + " inventory")
	all := HostedEdgePage{Items: []HostedEdge{}}
	after := ""
	for pageNumber := 0; pageNumber < 100; pageNumber++ {
		path := fmt.Sprintf("%s?limit=%d&after=%s", route, pageSize, url.QueryEscape(after))
		var page HostedEdgePage
		if err := c.do(ctx, http.MethodGet, path, nil, &page); err != nil {
			return HostedEdgePage{}, err
		}
		if page.ObservedAt.IsZero() || len(page.Items) > pageSize {
			return HostedEdgePage{}, invalid
		}
		if all.ObservedAt.IsZero() {
			all.ObservedAt = page.ObservedAt
		}
		for _, edge := range page.Items {
			if edge.ID == "" || len(edge.ID) > 128 || edge.ID <= after || edge.Name == "" || len(edge.Name) > 128 || (edge.Status != "ready" && edge.Status != "draining" && edge.Status != "unavailable") {
				return HostedEdgePage{}, invalid
			}
			after = edge.ID
			all.Items = append(all.Items, edge)
		}
		if page.NextCursor == "" {
			return all, nil
		}
		if len(page.Items) == 0 || page.NextCursor != after {
			return HostedEdgePage{}, errors.New("paperboat-server " + what + " inventory cursor did not advance")
		}
	}
	return HostedEdgePage{}, errors.New("paperboat-server " + what + " inventory exceeds the supported page limit")
}
