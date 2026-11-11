package server

import (
	"encoding/json"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/operation"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/recordingcontrol"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/recordings"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/session"
)

// RecordingControl decides and carries out recording starts and stops (recordingcontrol.Controller).
type RecordingControl interface {
	StartRecording(sessionID, actorAccountID string) (recordings.State, error)
	StopRecording(sessionID, actorAccountID string) (recordings.Meta, error)
	// Attached starts a rule recording when the attached person is recorded; it reports whether
	// one started.
	Attached(sessionID, accountID string) bool
}

// recordingAction starts or stops the session's recording for anyone attached to it with terminal
// access; the controller applies the team's rules on who may.
func (d *Dispatcher) recordingAction(authorization Authorization, request terminalRequest) operation.Outcome {
	if d.config.Recordings == nil {
		return failure("recording_unavailable")
	}
	if authorization.AccountID == "" || request.SessionID == "" {
		return failure("recording_forbidden")
	}
	if request.Action == "record-start" {
		state, err := d.config.Recordings.StartRecording(request.SessionID, authorization.AccountID)
		if err != nil {
			return failure(recordingcontrol.Code(err))
		}
		return result(struct {
			Recording recordings.State `json:"recording"`
		}{state})
	}
	if _, err := d.config.Recordings.StopRecording(request.SessionID, authorization.AccountID); err != nil {
		return failure(recordingcontrol.Code(err))
	}
	return result(struct{}{})
}

// attachedRecording starts a rule recording for the person who just attached, and shows it in their
// attach response, so REC is up before their first output.
func (d *Dispatcher) attachedRecording(authorization Authorization, sessionID string, response *terminalAttachResponse) {
	if d.config.Recordings == nil || !d.config.Recordings.Attached(sessionID, authorization.AccountID) {
		return
	}
	if snapshot, err := d.config.Sessions.Snapshot(sessionID); err == nil {
		response.Session.Snapshot.Recording = snapshot.Recording
	}
}

// recordingEvent tells the client when the session's recording started or stopped since this stream
// last said; attached clients show REC from it.
func (s *terminalOutputStream) recordingEvent(snapshot session.Snapshot) error {
	id := ""
	if snapshot.Recording != nil {
		id = snapshot.Recording.ID
	}
	if id == s.recordingID {
		return nil
	}
	s.recordingID = id
	payload, err := json.Marshal(struct {
		Event     string            `json:"event"`
		SessionID string            `json:"session_id"`
		Recording *recordings.State `json:"recording"`
	}{"recording", snapshot.ID, snapshot.Recording})
	if err != nil {
		return err
	}
	return &StreamEvent{Payload: payload}
}
