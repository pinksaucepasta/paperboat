package tunnel

import (
	"context"
	"crypto/rand"
	"github.com/pinksaucepasta/paperboat/internal/diagnostics"
	"github.com/pinksaucepasta/paperboat/internal/errorreport"
)

// Keep attempt timestamps and typed failures locally, without terminal content
// or arbitrary transport error strings, so recovery gaps can be accounted for.
func (c *ReconnectingConn) dialReconnect(ctx context.Context, caller, connection string) (Conn, error) {
	ctx = diagnostics.WithReconnect(ctx, rand.Text(), caller, connection)
	record := func(state string) { diagnostics.ReconnectEvent(ctx, "redial", state, nil) }
	record("started")
	next, err := c.reconnect(ctx)
	if err != nil {
		diagnostics.ReconnectEvent(ctx, "redial", "failed", err)
		if recorder := diagnostics.FromContext(ctx); recorder != nil {
			_ = recorder.RecordFault(errorreport.ProjectFault(ctx, "pb", "connect", "peer_connect", "terminal_reconnect_failed", err))
		}
	} else {
		record("ready")
	}
	return next, err
}

func (c *ReconnectingConn) recordOldLink(connection, caller, state string, err error) {
	ctx := diagnostics.WithReconnect(c.ctx, "old-link", caller, connection)
	diagnostics.ReconnectEvent(ctx, "old_connection", state, err)
}
