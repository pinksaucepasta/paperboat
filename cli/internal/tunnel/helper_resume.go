package tunnel

import (
	"context"
	"encoding/json"
	"fmt"
)

// resume takes the attachment this client held before over with the host's resume secret instead of an operation
// credential, from the delivered cursor; the response carries the rotated secret. A redial after a clean close needs
// no grant from the server, so it waits for no helper route and refreshes no network configuration.
func (c *helperTerminalConn) resume(ctx context.Context, secret []byte) error {
	fromSequence := uint64(max(0, c.target.AfterSequence))
	payload, _ := json.Marshal(map[string]any{"action": "attach", "session_id": c.target.SessionID, "attachment_id": c.target.InputAttachmentID, "from_sequence": fromSequence, "resume_secret": secret})
	c.target.ResumeSecret = nil // single use: a failed resume leaves nothing stale to retry, the CLI falls back to a grant
	frame, err := c.requestSync(ctx, "terminal.v1", payload)
	if err != nil {
		return fmt.Errorf("resume helper terminal attachment: %w", err)
	}
	return c.finishAttachment(frame, false, 0, fromSequence)
}
