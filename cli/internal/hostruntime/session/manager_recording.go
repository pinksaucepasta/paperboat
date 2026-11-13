package session

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	xterm "github.com/gitpod-io/xterm-go"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/recordings"
)

// ErrRecordingUnavailable: this host process was started without a recordings store.
var ErrRecordingUnavailable = errors.New("session recording is unavailable on this host")

// StartRecording records a running session from its current screen on. The caller has decided that
// the actor may start it; the request carries the team key and quota from the machine's policy.
func (m *Manager) StartRecording(sessionID string, request recordings.StartRequest) (recordings.State, error) {
	if m.config.Recordings == nil {
		return recordings.State{}, ErrRecordingUnavailable
	}
	session, err := m.get(sessionID)
	if err != nil {
		return recordings.State{}, err
	}
	session.opMu.Lock()
	for _, participant := range session.participants {
		request.Accounts = append(request.Accounts, participant.AccountID)
	}
	session.opMu.Unlock()
	prepared, err := m.config.Recordings.Prepare(sessionID, request)
	if err != nil {
		return recordings.State{}, err
	}
	// Under the output lock no PTY output falls between the screen the recording starts from and the
	// first output it receives.
	session.outputMu.Lock()
	session.opMu.Lock()
	state, _ := session.lifecycle.Snapshot()
	var result recordings.State
	if state != Running || session.screen == nil {
		err = ErrInvalidTransition
	} else {
		screen := xterm.NewSerializeAddon(session.screen).Serialize(nil)
		if !session.continuation.overflow {
			screen = append(screen, session.continuation.pending...)
		}
		dimensions := session.command.Dimensions
		result, err = m.config.Recordings.Activate(prepared, dimensions.Columns, dimensions.Rows, screen)
	}
	session.opMu.Unlock()
	session.outputMu.Unlock()
	if err != nil {
		m.config.Recordings.Abort(prepared)
		return recordings.State{}, err
	}
	return result, nil
}

// StopRecording ends a session's recording; the caller has decided that the actor may stop it.
func (m *Manager) StopRecording(sessionID, actorAccountID, reason string) (recordings.Meta, error) {
	if m.config.Recordings == nil {
		return recordings.Meta{}, ErrRecordingUnavailable
	}
	if _, err := m.get(sessionID); err != nil {
		return recordings.Meta{}, err
	}
	return m.config.Recordings.Stop(sessionID, actorAccountID, reason)
}

// Recordings lists every recording in this account's recordings directory, any generation's.
func (m *Manager) Recordings() ([]recordings.Meta, error) {
	if m.config.Recordings == nil {
		return nil, ErrRecordingUnavailable
	}
	return m.config.Recordings.List()
}

func (m *Manager) RecordingSegment(recordingID string, index uint32) ([]byte, error) {
	if m.config.Recordings == nil {
		return nil, ErrRecordingUnavailable
	}
	return m.config.Recordings.ReadSegment(recordingID, index)
}

func (m *Manager) PruneRecordings(retention time.Duration, quotaBytes int64) ([]recordings.Meta, error) {
	if m.config.Recordings == nil {
		return nil, ErrRecordingUnavailable
	}
	return m.config.Recordings.Prune(retention, quotaBytes)
}

// endRecording ends the recording of a session that is gone; having none is not an error.
func (m *Manager) endRecording(sessionID string) {
	if m.config.Recordings != nil {
		_, _ = m.config.Recordings.Stop(sessionID, "", recordings.ReasonSessionEnded)
	}
}

// CloseInterruptedRecording ends an open recording whose session the caller found no longer running.
func (m *Manager) CloseInterruptedRecording(recordingID string) (recordings.Meta, error) {
	metas, err := m.Recordings()
	if err != nil {
		return recordings.Meta{}, err
	}
	for _, meta := range metas {
		if meta.ID == recordingID {
			if active := m.config.Recordings.State(meta.SessionID); active != nil && active.ID == meta.ID {
				if live, err := m.get(meta.SessionID); err == nil {
					if state, _ := live.lifecycle.Snapshot(); state == Running || state == Closing {
						return meta, recordings.ErrRecording // this owner still records it
					}
				}
				// Its session is gone or over, so the recorder that was left active ends with it.
				return m.config.Recordings.Stop(meta.SessionID, "", recordings.ReasonSessionEnded)
			}
			return m.config.Recordings.CloseInterrupted(meta)
		}
	}
	return recordings.Meta{}, recordings.ErrNotFound
}

func (m *RemoteManager) StartRecording(sessionID string, request recordings.StartRequest) (recordings.State, error) {
	var out struct{ V0 recordings.State }
	err := m.invoke(context.Background(), "StartRecording", struct {
		Sessionid string
		Request   recordings.StartRequest
	}{sessionID, request}, &out)
	return out.V0, err
}

func (m *RemoteManager) StopRecording(sessionID, actorAccountID, reason string) (recordings.Meta, error) {
	var out struct{ V0 recordings.Meta }
	err := m.invoke(context.Background(), "StopRecording", struct {
		Sessionid, Actoraccountid, Reason string
	}{sessionID, actorAccountID, reason}, &out)
	return out.V0, err
}

func (m *RemoteManager) Recordings() ([]recordings.Meta, error) {
	var out struct{ V0 []recordings.Meta }
	err := m.invoke(context.Background(), "Recordings", struct{}{}, &out)
	return out.V0, err
}

func (m *RemoteManager) RecordingSegment(recordingID string, index uint32) ([]byte, error) {
	var out struct{ V0 []byte }
	err := m.invoke(context.Background(), "RecordingSegment", struct {
		Recordingid string
		Index       uint32
	}{recordingID, index}, &out)
	return out.V0, err
}

func (m *RemoteManager) PruneRecordings(retention time.Duration, quotaBytes int64) ([]recordings.Meta, error) {
	var out struct{ V0 []recordings.Meta }
	err := m.invoke(context.Background(), "PruneRecordings", struct {
		Retention  time.Duration
		Quotabytes int64
	}{retention, quotaBytes}, &out)
	return out.V0, err
}

func (m *RemoteManager) CloseInterruptedRecording(recordingID string) (recordings.Meta, error) {
	var out struct{ V0 recordings.Meta }
	err := m.invoke(context.Background(), "CloseInterruptedRecording", struct{ Recordingid string }{recordingID}, &out)
	return out.V0, err
}

func serveRecording(m Service, op string, body json.RawMessage) (json.RawMessage, error) {
	var args struct {
		Sessionid, Actoraccountid, Reason, Recordingid string
		Request                                        recordings.StartRequest
		Index                                          uint32
		Retention                                      time.Duration
		Quotabytes                                     int64
	}
	if err := decodeRemoteRequest(body, &args); err != nil {
		return nil, ErrInvalidSession
	}
	var value any
	var err error
	switch op {
	case "StartRecording":
		value, err = m.StartRecording(args.Sessionid, args.Request)
	case "StopRecording":
		value, err = m.StopRecording(args.Sessionid, args.Actoraccountid, args.Reason)
	case "Recordings":
		value, err = m.Recordings()
	case "RecordingSegment":
		value, err = m.RecordingSegment(args.Recordingid, args.Index)
	case "PruneRecordings":
		value, err = m.PruneRecordings(args.Retention, args.Quotabytes)
	default:
		value, err = m.CloseInterruptedRecording(args.Recordingid)
	}
	if err != nil {
		return nil, err
	}
	return json.Marshal(struct{ V0 any }{value})
}
