//go:build linux || darwin || windows

package updated

import "context"

// StartSessionOwner selects only a protected catalog generation. The updater
// derives the current executable and account from its installation declaration.
func (c *Client) StartSessionOwner(ctx context.Context, generation string) error {
	_, err := c.callRequest(ctx, ControlRequest{Schema: ControlProtocolV1, Operation: "session-owner-start", Generation: generation})
	return err
}
