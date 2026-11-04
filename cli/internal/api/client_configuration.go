package api

import (
	"context"
	clientapi "github.com/pinksaucepasta/paperboat/shared/client/api"
)

func (c *Client) ClientConfiguration(ctx context.Context) (clientapi.ClientConfiguration, error) {
	return clientapi.FetchClientConfiguration(ctx, c.do)
}
