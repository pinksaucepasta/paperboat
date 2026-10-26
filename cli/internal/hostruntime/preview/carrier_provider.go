package preview

import (
	"context"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/connector"
)

// AttachmentSession is the source-owned authenticated carrier projection.
// Identity must be the identity proven by the live connector-v1 session.
type AttachmentSession struct {
	Active   *connector.ActiveDataCarrier
	Identity connector.DataCarrierIdentity
	// Release drops this acquisition's ownership reference. The source must
	// make it idempotent and close an ephemeral transport only after its own
	// references reach zero.
	Release func(context.Context) error
}
