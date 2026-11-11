package server

import (
	"context"
	"encoding/binary"
	"encoding/json"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/auth"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/protocol"
	"github.com/pinksaucepasta/paperboat/shared/recording"

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
		return failure(recording.ErrorUnavailable)
	}
	if authorization.AccountID == "" || request.SessionID == "" {
		return failure(recording.ErrorForbidden)
	}
	if request.Action == recording.ActionStart {
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
	}{recording.EventRecording, snapshot.ID, snapshot.Recording})
	if err != nil {
		return err
	}
	return &StreamEvent{Payload: payload}
}

type recordingReadRequest struct {
	Action      string `json:"action"`
	RecordingID string `json:"recording_id"`
}

// recordingReadResult declares what the stream after it carries: Segments segment files, each with
// a 4-byte big-endian length before it, Bytes in all without the lengths.
type recordingReadResult struct {
	RecordingID string `json:"recording_id"`
	Segments    uint32 `json:"segments"`
	Bytes       int64  `json:"bytes"`
	Final       bool   `json:"final"`
}

// recordingRead answers a replay ticket's read of its one recording with the segments on disk now.
func (d *Dispatcher) recordingRead(authorization Authorization, payload json.RawMessage) operation.Outcome {
	var request recordingReadRequest
	claims, ok := authorization.Value.(auth.Claims)
	if decodeStrict(payload, &request) != nil || request.Action != recording.ReadAction || !ok || claims.CredentialClass != recording.CredentialClass || request.RecordingID == "" || request.RecordingID != authorization.ResourceID {
		return failure("not_found_or_forbidden")
	}
	metas, err := d.config.Sessions.Recordings()
	if err != nil {
		return failure(recording.ErrorUnavailable)
	}
	for _, meta := range metas {
		if meta.ID == request.RecordingID {
			return result(recordingReadResult{RecordingID: meta.ID, Segments: meta.Segments, Bytes: meta.Bytes, Final: meta.EndedAt != nil})
		}
	}
	return failure(recording.ErrorNotFound)
}

func (d *Dispatcher) openRecordingStream(authorization Authorization, outcome operation.Outcome) (OutputStream, bool, error) {
	var declared recordingReadResult
	if json.Unmarshal(outcome.Result, &declared) != nil || declared.RecordingID != authorization.ResourceID {
		return nil, false, ErrCredentialPolicy
	}
	return &recordingStream{sessions: d.config.Sessions, authorization: authorization, declared: declared}, true, nil
}

// recordingStream sends the declared segment files, ciphertext as stored, in 64 KiB frames whose
// StartSequence is the byte offset in the stream.
type recordingStream struct {
	sessions      session.Service
	authorization Authorization
	declared      recordingReadResult
	index         uint32
	pending       []byte
	offset        uint64
	sent          int64
}

func (s *recordingStream) Next(ctx context.Context) (protocol.BinaryFrame, error) {
	if err := ctx.Err(); err != nil {
		return protocol.BinaryFrame{}, err
	}
	if s.authorization.Revoked != nil && s.authorization.Revoked.Load() {
		return protocol.BinaryFrame{}, &StreamError{Code: "credential_revoked"}
	}
	if len(s.pending) == 0 {
		if s.index == s.declared.Segments {
			payload, _ := json.Marshal(struct {
				Event    string `json:"event"`
				Segments uint32 `json:"segments"`
			}{recording.EventStreamEnd, s.index})
			return protocol.BinaryFrame{}, &StreamEnd{Payload: payload}
		}
		segment, err := s.sessions.RecordingSegment(s.declared.RecordingID, s.index)
		if err != nil {
			// Retention or quota deleted it since the read was declared.
			return protocol.BinaryFrame{}, &StreamError{Code: recording.ErrorNotFound}
		}
		s.sent += int64(len(segment))
		if s.sent > s.declared.Bytes {
			return protocol.BinaryFrame{}, &StreamError{Code: "recording_changed"}
		}
		s.pending = binary.BigEndian.AppendUint32(make([]byte, 0, 4+len(segment)), uint32(len(segment)))
		s.pending = append(s.pending, segment...)
		s.index++
	}
	n := min(len(s.pending), 64<<10)
	frame := protocol.BinaryFrame{Channel: protocol.Stdout, StartSequence: s.offset, Data: s.pending[:n:n]}
	s.pending = s.pending[n:]
	s.offset += uint64(n)
	return frame, nil
}

func (s *recordingStream) Close() error { s.pending = nil; return nil }
