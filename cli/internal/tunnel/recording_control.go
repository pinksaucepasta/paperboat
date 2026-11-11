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

func (c *helperTerminalConn) RecordingSessionID() string { return c.target.SessionID }

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
	ctx, cancel := context.WithTimeout(ctx, helperRequestTimeout)
	defer cancel()
	select {
	case frame := <-response:
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
	case <-c.done:
		return nil, ErrTransportLost
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// RecordingRefusedError is the machine's refusal of a start or stop (a shared/recording error code).
type RecordingRefusedError struct{ Code string }

func (e *RecordingRefusedError) Error() string {
	return "the machine refused the recording request: " + e.Code
}
func (e *RecordingRefusedError) RemoteCode() string { return e.Code }
