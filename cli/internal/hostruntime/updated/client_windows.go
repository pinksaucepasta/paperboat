//go:build windows

package updated

import (
	"context"
	"time"

	"github.com/Microsoft/go-winio"
)

type Client struct {
	socketPath string
	timeout    time.Duration
}

func NewClient(socketPath string, timeout time.Duration) (*Client, error) {
	if !validPipePath(socketPath) || timeout <= 0 || timeout > maxUpdateControlTimeout {
		return nil, ErrInvalidControl
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
	dialCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	connection, err := winio.DialPipeContext(dialCtx, c.socketPath)
	if err != nil {
		return ControlResponse{}, controlClientFailure{kind: ErrUnavailable, cause: err}
	}
	return exchangeControl(ctx, connection, request, c.timeout, false)
}

func validPipePath(path string) bool {
	const prefix = `\\.\pipe\`
	return len(path) > len(prefix) && len(path) <= 256 && path[:len(prefix)] == prefix && !containsPipePathSeparator(path[len(prefix):])
}
func containsPipePathSeparator(value string) bool {
	for _, character := range value {
		if character == '/' || character == '\\' || character == ':' || character == '*' || character == '?' || character == '"' || character == '<' || character == '>' || character == '|' {
			return true
		}
	}
	return false
}
