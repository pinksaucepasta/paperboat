package server

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/recordingcontrol"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/recordings"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/session"
)

type fakeRecordingControl struct {
	actor   string
	refusal error
}

func (f *fakeRecordingControl) StartRecording(sessionID, actor string) (recordings.State, error) {
	f.actor = actor
	return recordings.State{ID: "rec_1", Trigger: recordings.TriggerManual, StartedBy: actor, StartedAt: time.Unix(1, 0)}, f.refusal
}
func (f *fakeRecordingControl) StopRecording(sessionID, actor string) (recordings.Meta, error) {
	f.actor = actor
	return recordings.Meta{}, f.refusal
}
func (f *fakeRecordingControl) Attached(string, string) bool { return false }

func TestRecordingActionsActAsTheAttachedAccount(t *testing.T) {
	control := &fakeRecordingControl{}
	d := &Dispatcher{config: DispatcherConfig{Recordings: control}}
	outcome := d.recordingAction(Authorization{AccountID: "acct_1"}, terminalRequest{Action: "record-start", SessionID: "s1"})
	var started struct {
		Recording recordings.State `json:"recording"`
	}
	if outcome.ErrorCode != "" || json.Unmarshal(outcome.Result, &started) != nil || started.Recording.StartedBy != "acct_1" || control.actor != "acct_1" {
		t.Fatalf("start %+v", outcome)
	}
	if outcome := d.recordingAction(Authorization{}, terminalRequest{Action: "record-start", SessionID: "s1"}); outcome.ErrorCode != "recording_forbidden" {
		t.Fatalf("an attachment without an account started a recording: %+v", outcome)
	}
	control.refusal = recordingcontrol.ErrForbidden
	if outcome := d.recordingAction(Authorization{AccountID: "acct_2"}, terminalRequest{Action: "record-stop", SessionID: "s1"}); outcome.ErrorCode != "recording_forbidden" {
		t.Fatalf("stop refusal %+v", outcome)
	}
	control.refusal = recordings.ErrNotRecording
	if outcome := d.recordingAction(Authorization{AccountID: "acct_2"}, terminalRequest{Action: "record-stop", SessionID: "s1"}); outcome.ErrorCode != "recording_inactive" {
		t.Fatalf("stop of nothing %+v", outcome)
	}
	if outcome := (&Dispatcher{}).recordingAction(Authorization{AccountID: "a"}, terminalRequest{Action: "record-start", SessionID: "s1"}); outcome.ErrorCode != "recording_unavailable" {
		t.Fatalf("a host without recordings %+v", outcome)
	}
	for _, role := range []TerminalRole{TerminalRoleOwner, TerminalRoleViewer, TerminalRoleInteractive} {
		if !browserTerminalActionAllowed(role, terminalRequest{Action: "record-start"}) || !sharedTerminalActionAllowed(role, terminalRequest{Action: "record-stop"}) {
			t.Fatalf("%s may not start or stop", role)
		}
	}
}

func TestRecordingEventIsSentOncePerChange(t *testing.T) {
	stream := &terminalOutputStream{}
	recorded := session.Snapshot{ID: "s1", Recording: &recordings.State{ID: "rec_1", Trigger: recordings.TriggerRule}}
	var event *StreamEvent
	if err := stream.recordingEvent(recorded); !errors.As(err, &event) {
		t.Fatalf("no event when recording started: %v", err)
	}
	var payload struct {
		Event     string            `json:"event"`
		SessionID string            `json:"session_id"`
		Recording *recordings.State `json:"recording"`
	}
	if json.Unmarshal(event.Payload, &payload) != nil || payload.Event != "recording" || payload.Recording == nil || payload.Recording.ID != "rec_1" {
		t.Fatalf("payload %s", event.Payload)
	}
	if err := stream.recordingEvent(recorded); err != nil {
		t.Fatalf("repeated event: %v", err)
	}
	if err := stream.recordingEvent(session.Snapshot{ID: "s1"}); !errors.As(err, &event) || json.Unmarshal(event.Payload, &payload) != nil || payload.Recording != nil {
		t.Fatalf("no stop event: %v", err)
	}
}
