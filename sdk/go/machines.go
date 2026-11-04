package paperboat

import (
	"context"

	"github.com/pinksaucepasta/paperboat/shared/client/api"
	"github.com/pinksaucepasta/paperboat/shared/client/discovery"
)

// These public names expose the shared inventory owner without copying its types.
type Machine = discovery.Machine
type MachinePage = discovery.MachinePage
type AmbiguousMachineError = discovery.AmbiguousMachineError

var ErrMachineNotFound = discovery.ErrMachineNotFound

// Machines returns the complete authorized inventory and its server-authored total.
func (c *Client) Machines(ctx context.Context) (MachinePage, error) {
	ctx, done, err := c.begin(ctx)
	if err != nil {
		return discovery.MachinePage{}, err
	}
	defer done()
	return discovery.Machines(ctx, c.inventoryRequest())
}

// Machine resolves an ID or exact unique name once. Retain the returned ID for subsequent work.
func (c *Client) Machine(ctx context.Context, idOrName string) (Machine, error) {
	ctx, done, err := c.begin(ctx)
	if err != nil {
		return discovery.Machine{}, err
	}
	defer done()
	return discovery.Resolve(ctx, c.inventoryRequest(), idOrName)
}
func (c *Client) inventoryRequest() api.Request {
	return api.Request{Do: c.api.Do, InvalidResponse: func(err error) error {
		return &api.CallFailure{Message: "paperboat-server returned invalid inventory pagination", Err: err}
	}}
}
