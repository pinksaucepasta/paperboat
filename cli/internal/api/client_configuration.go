package api

import (
	"context"
	"github.com/pinksaucepasta/paperboat/internal/clientapi"
)

func (c *Client) ClientConfiguration(ctx context.Context) (clientapi.ClientConfiguration, error) {
	return clientapi.FetchClientConfiguration(ctx, c.do)
}
