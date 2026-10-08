package api

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// collectOffsetInventory keeps selectors complete while rejecting a server page
// which repeats its position or claims more results without returning any.
func collectOffsetInventory[T any](ctx context.Context, c *Client, path, field string, filters url.Values) ([]T, error) {
	const limit = 200
	items := []T{}
	for offset := 0; ; {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		query := url.Values{}
		for key, values := range filters {
			query[key] = append([]string(nil), values...)
		}
		query.Set("limit", strconv.Itoa(limit))
		query.Set("offset", strconv.Itoa(offset))
		var page struct {
			Items      []T        `json:"items"`
			Requests   []T        `json:"requests"`
			Sessions   []T        `json:"sessions"`
			Pagination Pagination `json:"pagination"`
		}
		if err := c.do(ctx, http.MethodGet, path+"?"+query.Encode(), nil, &page); err != nil {
			return nil, err
		}
		batch := page.Items
		if field == "requests" {
			batch = page.Requests
		}
		if field == "sessions" {
			batch = page.Sessions
		}
		if len(batch) > limit {
			return nil, errors.New("inventory page exceeds the requested limit")
		}
		items = append(items, batch...)
		if page.Pagination.NextOffset == nil {
			return items, nil
		}
		next := *page.Pagination.NextOffset
		if page.Pagination.Offset != offset || page.Pagination.Limit != limit || len(batch) == 0 || next <= offset || next != offset+len(batch) || next >= page.Pagination.Total {
			return nil, errors.New("inventory pagination did not advance consistently")
		}
		offset = next
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
