//go:build darwin || linux

package updated

import (
	"context"
	"net"
	"path/filepath"
	"time"
)

// Client exposes only the fixed local updater operations. It cannot submit a
// binary, a release URL, or an activation command.
type Client struct {
	socketPath string
	timeout    time.Duration
}

func NewClient(socketPath string, timeout time.Duration) (*Client, error) {
	if !filepath.IsAbs(socketPath) || timeout <= 0 || timeout > maxUpdateControlTimeout {
		return nil, ErrInvalidConfig
	}
	return &Client{socketPath: socketPath, timeout: timeout}, nil
}

func (c *Client) Status(ctx context.Context) (ControlResponse, error) { return c.call(ctx, "status") }
func (c *Client) Check(ctx context.Context) (ControlResponse, error)  { return c.call(ctx, "check") }
func (c *Client) Download(ctx context.Context) (ControlResponse, error) {
	return c.call(ctx, "download")
}
func (c *Client) Install(ctx context.Context, approvalID string) (ControlResponse, error) {
	return c.callRequest(ctx, ControlRequest{Schema: ControlProtocolV1, Operation: "install", ApprovalID: approvalID})
}

func (c *Client) call(ctx context.Context, operation string) (ControlResponse, error) {
	return c.callRequest(ctx, ControlRequest{Schema: ControlProtocolV1, Operation: operation})
}

func (c *Client) callRequest(ctx context.Context, request ControlRequest) (ControlResponse, error) {
	if c == nil || !validControlRequest(request) {
		return ControlResponse{}, ErrInvalidControl
	}
	if ctx == nil {
		ctx = context.Background()
	}
	connection, err := (&net.Dialer{Timeout: c.timeout}).DialContext(ctx, "unix", c.socketPath)
	if err != nil {
		return ControlResponse{}, controlClientFailure{kind: ErrUnavailable, cause: err}
	}
	return exchangeControl(ctx, connection, request, c.timeout, true)
}
