package tunnel

import (
	"context"
	"encoding/json"
	"errors"
)

var ErrTerminalViewOnly = errors.New("this terminal is view-only; ask the session owner for interactive access")

// Shared attachments never create or restart the owner's shell. A zero cursor
// asks the runtime for its atomic bounded recent tail, including pre-join output.
func (c *helperTerminalConn) initializeShared(ctx context.Context) error {
	if c.target.SessionID == "" {
		return errors.New("shared terminal descriptor is missing session ID")
	}
	from := uint64(max(0, c.target.AfterSequence))
	attach := func(cursor uint64) (helperFrame, error) {
		payload, _ := json.Marshal(map[string]any{"action": "attach", "session_id": c.target.SessionID, "attachment_id": c.target.InputAttachmentID, "from_sequence": cursor})
		return c.requestSync(ctx, "terminal.v1", payload)
	}
	frame, err := attach(from)
	var remote *helperRemoteError
	if from > 0 && errors.As(err, &remote) && remote.Code == "replay_gap" && remote.Details != nil && remote.Details.EarliestSequence <= remote.Details.LatestSequence {
		if c.target.ReplayGapSink != nil {
			c.target.ReplayGapSink(remote.Details.RequestedSequence, remote.Details.EarliestSequence, remote.Details.LatestSequence)
		}
		// Do not advance the committed cursor past unread retained output.
		c.initial = append(c.initial, helperOutput{data: []byte(helperReplayGapMarker)})
		from = 0
		frame, err = attach(0)
	}
	if err != nil {
		return err
	}
	return c.finishAttachment(frame, false, 0, from)
}
