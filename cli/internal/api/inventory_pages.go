package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// collectOffsetInventory keeps selectors complete while rejecting a server page
// which repeats its position or claims more results without returning any.
func collectOffsetInventory[T any](ctx context.Context, c *Client, path, field string, filters url.Values) ([]T, error) {
	items, _, err := collectOffsetInventoryPage[T](ctx, c, path, field, filters, 0)
	return items, err
}

// collectOffsetInventoryPage reads at most maxItems items (0: every page) and the server's total, so a list
// command shows one bounded page with its total instead of loading everything (PQ-04).
func collectOffsetInventoryPage[T any](ctx context.Context, c *Client, path, field string, filters url.Values, maxItems int) ([]T, int, error) {
	limit := 200
	if maxItems > 0 && maxItems < limit {
		limit = maxItems
	}
	items := []T{}
	for offset := 0; ; {
		if err := ctx.Err(); err != nil {
			return nil, 0, err
		}
		query := url.Values{}
		for key, values := range filters {
			query[key] = append([]string(nil), values...)
		}
		query.Set("limit", strconv.Itoa(limit))
		query.Set("offset", strconv.Itoa(offset))
		var page struct {
			Items      *[]T `json:"items"`
			Requests   *[]T `json:"requests"`
			Sessions   *[]T `json:"sessions"`
			Pagination *struct {
				Limit      *int            `json:"limit"`
				Offset     *int            `json:"offset"`
				Total      *int            `json:"total"`
				NextOffset json.RawMessage `json:"next_offset"`
			} `json:"pagination"`
		}
		if err := c.do(ctx, http.MethodGet, path+"?"+query.Encode(), nil, &page); err != nil {
			return nil, 0, err
		}
		batch := page.Items
		if field == "requests" {
			batch = page.Requests
		}
		if field == "sessions" {
			batch = page.Sessions
		}
		invalid := func() error { return &ResponseDecodeError{Err: errors.New("invalid inventory pagination")} }
		if batch == nil || page.Pagination == nil || page.Pagination.Limit == nil || page.Pagination.Offset == nil || page.Pagination.Total == nil || len(page.Pagination.NextOffset) == 0 {
			return nil, 0, invalid()
		}
		p := page.Pagination
		var next *int
		if json.Unmarshal(p.NextOffset, &next) != nil || *p.Limit != limit || *p.Offset != offset || *p.Total < 0 || int64(*p.Total) > 2147483647 || len(*batch) > limit {
			return nil, 0, invalid()
		}
		end := int64(offset) + int64(len(*batch))
		if end > int64(*p.Total) || next == nil && end != int64(*p.Total) || next != nil && (len(*batch) == 0 || int64(*next) != end || end >= int64(*p.Total)) {
			return nil, 0, invalid()
		}
		items = append(items, (*batch)...)
		if next == nil || maxItems > 0 && len(items) >= maxItems {
			if maxItems > 0 && len(items) > maxItems {
				items = items[:maxItems]
			}
			return items, *p.Total, nil
		}
		offset = *next
	}
}

func collectCursorInventory[T any](ctx context.Context, c *Client, path string) ([]T, error) {
	const limit = 100
	items := []T{}
	cursor := ""
	seen := map[string]bool{"": true}
	for {
		query := url.Values{"limit": {strconv.Itoa(limit)}}
		if cursor != "" {
			query.Set("cursor", cursor)
		}
		var page struct {
			Items      []T    `json:"items"`
			NextCursor string `json:"next_cursor"`
		}
		if err := c.do(ctx, http.MethodGet, path+"?"+query.Encode(), nil, &page); err != nil {
			return nil, err
		}
		if len(page.Items) > limit || len(page.NextCursor) > 4096 || strings.ContainsAny(page.NextCursor, "\x00\r\n") {
			return nil, errors.New("invalid inventory cursor page")
		}
		items = append(items, page.Items...)
		if page.NextCursor == "" {
			return items, nil
		}
		if len(page.Items) == 0 || seen[page.NextCursor] {
			return nil, errors.New("inventory cursor did not advance")
		}
		seen[page.NextCursor] = true
		cursor = page.NextCursor
	}
}
