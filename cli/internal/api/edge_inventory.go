package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

type HostedEdge struct {
	ID            string     `json:"id"`
	Region        string     `json:"region"`
	Status        string     `json:"status"`
	LastHeartbeat *time.Time `json:"last_heartbeat_at,omitempty"`
}

type HostedEdgePage struct {
	Items      []HostedEdge `json:"items"`
	NextCursor string       `json:"next_cursor,omitempty"`
	ObservedAt time.Time    `json:"observed_at"`
}

func (c *Client) ListHostedEdges(ctx context.Context) (HostedEdgePage, error) {
	const pageSize = 100
	all := HostedEdgePage{Items: []HostedEdge{}}
	after := ""
	for pageNumber := 0; pageNumber < 100; pageNumber++ {
		path := fmt.Sprintf("/v1/edges?limit=%d&after=%s", pageSize, url.QueryEscape(after))
		var page HostedEdgePage
		if err := c.do(ctx, http.MethodGet, path, nil, &page); err != nil {
			return HostedEdgePage{}, err
		}
		if page.ObservedAt.IsZero() || len(page.Items) > pageSize {
			return HostedEdgePage{}, errors.New("paperboat-server returned invalid edge inventory")
		}
		if all.ObservedAt.IsZero() {
			all.ObservedAt = page.ObservedAt
		}
		for _, edge := range page.Items {
			if edge.ID == "" || len(edge.ID) > 128 || edge.ID <= after || len(edge.Region) > 128 || (edge.Status != "ready" && edge.Status != "draining" && edge.Status != "unavailable") {
				return HostedEdgePage{}, errors.New("paperboat-server returned invalid edge inventory")
			}
			after = edge.ID
			all.Items = append(all.Items, edge)
		}
		if page.NextCursor == "" {
			return all, nil
		}
		if len(page.Items) == 0 || page.NextCursor != after {
			return HostedEdgePage{}, errors.New("paperboat-server edge inventory cursor did not advance")
		}
	}
	return HostedEdgePage{}, errors.New("paperboat-server edge inventory exceeds the supported page limit")
}
