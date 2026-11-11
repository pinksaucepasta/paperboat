package server

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/auth"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/operation"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/recordingcontrol"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/recordings"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/session"
	wire "github.com/pinksaucepasta/paperboat/shared/recording"
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

type replaySessions struct {
	session.Service
	segments map[uint32][]byte
}

func (s replaySessions) Recordings() ([]recordings.Meta, error) {
	return []recordings.Meta{{ID: replayID, Segments: 2, Bytes: int64(len(s.segments[0]) + len(s.segments[1]))}}, nil
}
func (s replaySessions) RecordingSegment(id string, index uint32) ([]byte, error) {
	if segment, ok := s.segments[index]; ok && id == replayID {
		return segment, nil
	}
	return nil, recordings.ErrNotFound
}

const replayID = "rec_00000000-0000-4000-8000-000000000001"

// A replay ticket reads only its own recording, and the stream carries the declared segment files
// behind their lengths, in order, then the end event.
func TestReplayTicketReadsOnlyItsRecording(t *testing.T) {
	sessions := replaySessions{segments: map[uint32][]byte{0: bytes.Repeat([]byte("a"), 70<<10), 1: []byte("bb")}}
	d := &Dispatcher{config: DispatcherConfig{Sessions: sessions}}
	ticket := Authorization{ResourceID: replayID, Value: auth.Claims{CredentialClass: wire.CredentialClass}}
	read := func(authorization Authorization, id string) operation.Outcome {
		payload, _ := json.Marshal(map[string]string{"action": wire.ReadAction, "recording_id": id})
		return d.recordingRead(authorization, payload)
	}
	if outcome := read(ticket, "rec_00000000-0000-4000-8000-000000000002"); outcome.ErrorCode != "not_found_or_forbidden" {
		t.Fatalf("another recording: %+v", outcome)
	}
	if outcome := read(Authorization{ResourceID: replayID, Value: auth.Claims{CredentialClass: "terminal_operation"}}, replayID); outcome.ErrorCode != "not_found_or_forbidden" {
		t.Fatalf("a terminal credential read a recording: %+v", outcome)
	}
	outcome := read(ticket, replayID)
	stream, ok, err := d.openRecordingStream(ticket, outcome)
	if outcome.ErrorCode != "" || !ok || err != nil {
		t.Fatalf("read %+v %v", outcome, err)
	}
	var got []byte
	for {
		frame, err := stream.Next(context.Background())
		var end *StreamEnd
		if errors.As(err, &end) {
			break
		}
		if err != nil || frame.StartSequence != uint64(len(got)) || len(frame.Data) > 64<<10 {
			t.Fatalf("frame at %d: %v", len(got), err)
		}
		got = append(got, frame.Data...)
	}
	var want []byte
	for i := range uint32(2) {
		want = binary.BigEndian.AppendUint32(want, uint32(len(sessions.segments[i])))
		want = append(want, sessions.segments[i]...)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("stream %d bytes, want %d", len(got), len(want))
	}

	// A paced reader asks for one segment at a time and gets exactly that range.
	ranged := func(from, max uint32) operation.Outcome {
		payload, _ := json.Marshal(map[string]any{"action": wire.ReadAction, "recording_id": replayID, "from_segment": from, "max_segments": max})
		return d.recordingRead(ticket, payload)
	}
	outcome = ranged(1, 1)
	var declared recordingReadResult
	if outcome.ErrorCode != "" || json.Unmarshal(outcome.Result, &declared) != nil || declared.FromSegment != 1 || declared.Segments != 1 || declared.TotalSegments != 2 || declared.Bytes != 2 {
		t.Fatalf("ranged read %+v %s", outcome, outcome.Result)
	}
	stream, _, _ = d.openRecordingStream(ticket, outcome)
	frame, err := stream.Next(context.Background())
	if err != nil || !bytes.Equal(frame.Data, []byte{0, 0, 0, 2, 'b', 'b'}) {
		t.Fatalf("ranged frame %v %v", frame.Data, err)
	}
	var end *StreamEnd
	if _, err := stream.Next(context.Background()); !errors.As(err, &end) {
		t.Fatalf("ranged stream did not end: %v", err)
	}
	for _, r := range [][2]uint32{{3, 1}, {0, maxRangedSegments + 1}, {1, 0}} {
		if outcome := ranged(r[0], r[1]); outcome.ErrorCode != "invalid_request" {
			t.Fatalf("range %v: %+v", r, outcome)
		}
	}
}
