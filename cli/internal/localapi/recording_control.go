package localapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	wire "github.com/pinksaucepasta/paperboat/shared/recording"
)

// RecordingControlRequest starts or stops the recording of a terminal session this computer is attached
// to (pb connect). An empty SessionID means the only attached session.
type RecordingControlRequest struct {
	SessionID string `json:"session_id,omitempty"`
	Action    string `json:"action"`
}

// RecordingControlResult is the session acted on and its recording now (nil after a stop).
type RecordingControlResult struct {
	SessionID string      `json:"session_id"`
	Recording *wire.State `json:"recording"`
}

// Recording control refusals of the daemon itself; the host's refusals keep their shared codes.
const (
	RecordingNotAttachedCode = "recording_session_not_attached"
	RecordingAmbiguousCode   = "recording_session_ambiguous"
)

// RecordingControlError is a refusal with a code the CLI explains.
type RecordingControlError struct{ Code, Message string }

func (e *RecordingControlError) Error() string { return e.Message }

func (r RecordingControlRequest) valid() bool {
	return (r.Action == wire.ActionStart || r.Action == wire.ActionStop) && (r.SessionID == "" || safeValue(r.SessionID))
}

func (s *Server) recordingControl(writer http.ResponseWriter, request *http.Request, requestID string, peer Peer) {
	if request.Method != http.MethodPost || request.URL.RawQuery != "" || request.Header.Get("Content-Type") != "application/json" || request.ContentLength < 0 || request.ContentLength > maxJSONBytes || s.config.RecordingControl == nil {
		writeError(writer, http.StatusBadRequest, requestID, "invalid_request", "recording control request is invalid")
		return
	}
	var value RecordingControlRequest
	if decodeStrictJSON(io.LimitReader(request.Body, maxJSONBytes+1), &value) != nil || !value.valid() {
		writeError(writer, http.StatusBadRequest, requestID, "invalid_request", "recording control request is invalid")
		return
	}
	result, err := s.config.RecordingControl(request.Context(), peer, value)
	if err != nil {
		var refused *RecordingControlError
		var coded interface{ RemoteCode() string }
		switch {
		case errors.As(err, &refused):
			writeError(writer, http.StatusConflict, requestID, refused.Code, refused.Message)
		case errors.As(err, &coded) && recordingRefusal(coded.RemoteCode()):
			writeError(writer, http.StatusForbidden, requestID, coded.RemoteCode(), "the machine refused the recording request")
		default:
			writeError(writer, http.StatusServiceUnavailable, requestID, "recording_control_unavailable", "the machine could not be reached; check the terminal connection")
		}
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("X-Paperboat-Protocol", ProtocolV1)
	_ = json.NewEncoder(writer).Encode(result)
}

// RecordingControl asks the daemon to start or stop the recording of an attached session.
func (c *Client) RecordingControl(ctx context.Context, value RecordingControlRequest) (RecordingControlResult, error) {
	var result RecordingControlResult
	body, err := json.Marshal(value)
	if c == nil || ctx == nil || !value.valid() || err != nil {
		return result, ErrInvalidConfig
	}
	requestCtx, cancel := context.WithTimeout(ctx, 40*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodPost, "http://paperboat.local/v1/recording-control", bytes.NewReader(body))
	if err != nil {
		return result, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Paperboat-Request-ID", localRequestID())
	response, err := c.http.Do(request)
	if err != nil {
		return result, err
	}
	defer response.Body.Close()
	if response.Header.Get("X-Paperboat-Protocol") != ProtocolV1 {
		return result, ErrVersionMismatch
	}
	if response.StatusCode != http.StatusOK {
		return result, decodeRemoteError(response)
	}
	if decodeStrictJSON(io.LimitReader(response.Body, maxJSONBytes+1), &result) != nil || !safeValue(result.SessionID) || result.Recording != nil && !result.Recording.Valid() {
		return RecordingControlResult{}, ErrInvalidResponse
	}
	return result, nil
}

func recordingRefusal(code string) bool {
	switch code {
	case wire.ErrorUnavailable, wire.ErrorForbidden, wire.ErrorActive, wire.ErrorInactive, "not_found_or_forbidden":
		return true
	}
	return false
}
