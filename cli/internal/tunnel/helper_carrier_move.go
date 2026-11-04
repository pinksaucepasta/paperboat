package tunnel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"time"
)

// closeForCarrierMove does not detach the logical attachment: the replacement
// takes it over after the old reader has joined. Thus no old detach can arrive
// after a new attach and remove it. The authenticated offer/ACK stream stays up.
func (c *helperTerminalConn) closeForCarrierMove() error {
	message, ok := c.message.(*nativeMessageConnection)
	if !ok {
		return ErrPeerTerminalInvalid
	}
	if c.closing.Swap(true) {
		return net.ErrClosed
	}
	message.closeOnce.Do(func() {
		message.cancel()
		for _, stream := range []nativeStream{message.control, message.input, message.output} {
			if stream == nil {
				continue
			}
			if conn, ok := stream.(net.Conn); ok {
				_ = conn.SetDeadline(time.Now())
			}
			_ = stream.Close()
		}
	})
	c.finish(1, errors.Join(ErrTransportLost, net.ErrClosed))
	return nil
}

// terminalResumeKey carries a carrier move's resume secret to the attachment it moves.
type terminalResumeKey struct{}

// resume takes this connection's attachment over with the host's resume secret instead of an
// operation credential, from the delivered cursor; the response carries the rotated secret.
func (c *helperTerminalConn) resume(ctx context.Context, secret []byte) error {
	fromSequence := uint64(max(0, c.target.AfterSequence))
	payload, _ := json.Marshal(map[string]any{"action": "attach", "session_id": c.target.SessionID, "attachment_id": c.target.InputAttachmentID, "from_sequence": fromSequence, "resume_secret": secret})
	frame, err := c.requestSync(ctx, "terminal.v1", payload)
	if err != nil {
		return fmt.Errorf("resume helper terminal attachment: %w", err)
	}
	return c.finishAttachment(frame, false, 0, fromSequence)
}
