package api

import (
	"context"
	clientapi "github.com/pinksaucepasta/paperboat/shared/client/api"
	"net/url"
)

func (c *Client) inventoryRequest() clientapi.Request {
	return clientapi.Request{Do: c.do, InvalidResponse: func(err error) error { return &ResponseDecodeError{Err: err} }}
}
func collectOffsetInventory[T any](ctx context.Context, c *Client, path, field string, filters url.Values) ([]T, error) {
	return clientapi.CollectOffsetInventory[T](ctx, c.inventoryRequest(), path, field, filters)
}
func collectOffsetInventoryPage[T any](ctx context.Context, c *Client, path, field string, filters url.Values, maxItems int) ([]T, int, error) {
	return clientapi.CollectOffsetInventoryPage[T](ctx, c.inventoryRequest(), path, field, filters, maxItems)
}
func collectCursorInventory[T any](ctx context.Context, c *Client, path string) ([]T, error) {
	return clientapi.CollectCursorInventory[T](ctx, c.inventoryRequest(), path)
}
func collectKeysetPage[T any](ctx context.Context, c *Client, path string, filters url.Values, maxItems int) ([]T, int, error) {
	return clientapi.CollectKeysetPage[T](ctx, c.inventoryRequest(), path, filters, maxItems)
}
