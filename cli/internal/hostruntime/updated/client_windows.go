//go:build windows

package updated

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
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
		return ControlResponse{}, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	defer connection.Close()
	deadline := time.Now().Add(c.timeout)
	if limit, ok := ctx.Deadline(); ok && limit.Before(deadline) {
		deadline = limit
	}
	_ = connection.SetDeadline(deadline)
	if err := json.NewEncoder(connection).Encode(request); err != nil {
		return ControlResponse{}, err
	}
	decoder := json.NewDecoder(io.LimitReader(connection, 16<<10))
	decoder.DisallowUnknownFields()
	var response ControlResponse
	var extra any
	if decoder.Decode(&response) != nil || decoder.Decode(&extra) != io.EOF || response.Schema != ControlProtocolV1 || (response.Status != "ok" && response.Status != "error") || !validControlResponseError(response.Status, response.ErrorCode, response.ErrorMessage) {
		return ControlResponse{}, ErrInvalidControl
	}
	if response.ErrorCode != "" {
		return ControlResponse{}, &ControlError{Code: response.ErrorCode, Message: response.ErrorMessage}
	}
	return response, nil
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
