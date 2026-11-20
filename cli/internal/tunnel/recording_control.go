package tunnel

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	wire "github.com/pinksaucepasta/paperboat/shared/recording"
)

// RecordingController is an attached terminal link that starts or stops its session's recording as the
// attachment's account, the same control the browser's REC button sends.
type RecordingController interface {
	RecordingSessionID() string
	RecordingControl(ctx context.Context, action string) (*wire.State, error)
}

// The native terminal pb connect carries is a cursorTerminal around its helper link, and the daemon registers the
// terminal it dialed: both types must be controllers, or `pb recordings start|stop` never finds the attachment.
var (
	_ RecordingController = (*helperTerminalConn)(nil)
	_ RecordingController = (*cursorTerminal)(nil)
)

func (c *helperTerminalConn) RecordingSessionID() string { return c.target.SessionID }

func (c *cursorTerminal) RecordingSessionID() string { return c.helper.RecordingSessionID() }

func (c *cursorTerminal) RecordingControl(ctx context.Context, action string) (*wire.State, error) {
	return c.helper.RecordingControl(ctx, action)
}

// RecordingControl sends record-start or record-stop on this attachment and returns the recording
// that runs now (nil after a stop). Refusals come back as the host's error code.
func (c *helperTerminalConn) RecordingControl(ctx context.Context, action string) (*wire.State, error) {
	if action != wire.ActionStart && action != wire.ActionStop {
		return nil, ErrPeerTerminalInvalid
	}
	id := helperID("request")
	response := make(chan helperFrame, 1)
	c.pendingMu.Lock()
	c.responses[id] = response
	c.pendingMu.Unlock()
	defer func() {
		c.pendingMu.Lock()
		delete(c.responses, id)
		c.pendingMu.Unlock()
	}()
	payload, err := json.Marshal(map[string]string{"action": action, "session_id": c.target.SessionID})
	if err != nil {
		return nil, err
	}
	if err := c.writeFrame(helperFrame{Type: "request", RequestID: id, Version: helperProtocolVersion, OperationID: helperID("operation"), Capability: "terminal.v1", DeadlineMS: uint32(helperRequestTimeout / time.Millisecond), Payload: payload}); err != nil {
		return nil, err
	}
	return c.recordingOutcome(ctx, action, response)
}

// recordingOutcome waits for the host's answer to a record-start or record-stop. An answer that was really received
// wins over a closed link or a canceled ctx: both can be ready together and a random pick would refuse a delivered
// start or stop.
func (c *helperTerminalConn) recordingOutcome(ctx context.Context, action string, response <-chan helperFrame) (*wire.State, error) {
	ctx, cancel := context.WithTimeout(ctx, helperRequestTimeout)
	defer cancel()
	decode := func(frame helperFrame) (*wire.State, error) {
		if frame.Type == "error" {
			err := decodeHelperError(frame)
			var remote *helperRemoteError
			if errors.As(err, &remote) {
				return nil, &RecordingRefusedError{Code: remote.Code}
			}
			return nil, err
		}
		var answer struct {
			Result struct {
				Recording *wire.State `json:"recording"`
			} `json:"result"`
		}
		if frame.Type != "response" || json.Unmarshal(frame.Payload, &answer) != nil || answer.Result.Recording != nil && !answer.Result.Recording.Valid() || (action == wire.ActionStart) != (answer.Result.Recording != nil) {
			return nil, errors.New("the machine returned an invalid recording answer")
		}
		return answer.Result.Recording, nil
	}
	select {
	case frame := <-response:
		return decode(frame)
	case <-c.done:
		if frame, ok := readyFrame(response); ok {
			return decode(frame)
		}
		return nil, ErrTransportLost
	case <-ctx.Done():
		if frame, ok := readyFrame(response); ok {
			return decode(frame)
		}
		return nil, ctx.Err()
	}
}

// RecordingRefusedError is the machine's refusal of a start or stop (a shared/recording error code).
type RecordingRefusedError struct{ Code string }

func (e *RecordingRefusedError) Error() string {
	return "the machine refused the recording request: " + e.Code
}
func (e *RecordingRefusedError) RemoteCode() string { return e.Code }
