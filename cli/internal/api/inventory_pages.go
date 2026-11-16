package api

import (
	"context"
	clientapi "github.com/pinksaucepasta/paperboat/shared/client/api"
	"net/url"
)

func (c *Client) inventoryRequest() clientapi.Request {
	return clientapi.Request{Do: c.do, InvalidResponse: func(err error) error { return &ResponseDecodeError{Err: err} }}
}

// retryingInventoryRequest reads list pages for a command a person or script waits on: a 503/429 or a lost answer is
// asked again with bounded backoff (F21-164: 14 of 91 session cleanups failed on one `pb session list` 503 while the
// server was busy). The daemon's background refresh keeps inventoryRequest: it retries on its own outage backoff and
// reports one fault per refresh.
func (c *Client) retryingInventoryRequest() clientapi.Request {
	request := c.inventoryRequest()
	request.Do = func(ctx context.Context, method, path string, body, out any) error {
		return c.doIssuance(ctx, method, path, body, out, false)
	}
	return request
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
