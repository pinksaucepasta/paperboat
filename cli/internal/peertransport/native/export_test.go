package native

import (
	"context"

	"github.com/pinksaucepasta/paperboat/internal/peertransport/mesh"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/tailnet"
)

// SetNewCarrierNetwork replaces how the owner creates a carrier network, so a test can make one hang.
func (o *Owner) SetNewCarrierNetwork(f func(context.Context, [32]byte, func(mesh.ConnPacketConn)) (*tailnet.CarrierNetwork, error)) {
	o.newCarrierNetwork = f
}
