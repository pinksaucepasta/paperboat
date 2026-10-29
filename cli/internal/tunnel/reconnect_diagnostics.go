package tunnel

import (
	"context"
	"github.com/pinksaucepasta/paperboat/internal/diagnostics"
	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/shared/supportref"
)

// Keep attempt timestamps and typed failures locally, without terminal content
// or arbitrary transport error strings, so recovery gaps can be accounted for.
func (c *ReconnectingConn) dialReconnect(ctx context.Context) (Conn, error) {
	recorder := diagnostics.FromContext(ctx)
	record := func(state string) {
		if recorder != nil {
			_ = recorder.RecordWithSupportReference("terminal", "redial", "info", supportref.FromContext(ctx), map[string]string{"operation": "connect", "state": state})
		}
	}
	record("started")
	next, err := c.reconnect(ctx)
	if err != nil {
		record("failed")
		if recorder != nil {
			_ = recorder.RecordFault(errorreport.ProjectFault(ctx, "pb", "connect", "peer_connect", "terminal_reconnect_failed", err))
		}
	} else {
		record("ready")
	}
	return next, err
}
