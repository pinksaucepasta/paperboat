package main

import (
	"encoding/json"
	"strings"
	"testing"

	wire "github.com/pinksaucepasta/paperboat/shared/recording"
)

func TestRecordingModeOffersOnlyRecordingReads(t *testing.T) {
	var hello struct {
		Capabilities []string `json:"capabilities"`
	}
	if json.Unmarshal(helloPayload(connectionModeRecording), &hello) != nil || strings.Join(hello.Capabilities, ",") != "terminal.v1,health.v1,recording.v1" {
		t.Fatalf("recording hello=%v", hello.Capabilities)
	}
	if _, _, ok := callRequestFrame("call_request_1", "operation_12345678", wire.Capability, json.RawMessage(`{"action":"read","recording_id":"rec_00000000-0000-4000-8000-000000000001"}`)); !ok {
		t.Fatal("recording read frame refused")
	}
}

// The attach event carries the session's recording so the page shows REC before the first output; an
// invalid recording fails the attach instead of hiding REC.
func TestAttachResponseCarriesTheRecording(t *testing.T) {
	attach := func(recording string) string {
		return `{"result":{"stream_id":7,"attachment_id":"att","attachment_lease":1,"session":{"snapshot":{"generation":1,"dimensions":{"columns":80,"rows":24}` + recording + `}}}}`
	}
	state, err := parseAttachResponse([]byte(attach("")))
	if err != nil || state.Recording != nil || state.recordingJSON() != nil {
		t.Fatalf("not recording: %+v %v", state.Recording, err)
	}
	state, err = parseAttachResponse([]byte(attach(`,"recording":{"id":"rec_00000000-0000-4000-8000-000000000001","trigger":"rule","started_at":"2026-11-11T00:00:00Z"}`)))
	if err != nil || state.Recording == nil || state.Recording.Trigger != wire.TriggerRule {
		t.Fatalf("recording: %+v %v", state.Recording, err)
	}
	var shown wire.State
	if raw, ok := state.recordingJSON().(json.RawMessage); !ok || json.Unmarshal(raw, &shown) != nil || shown.ID != state.Recording.ID {
		t.Fatalf("attach event recording %v", state.recordingJSON())
	}
	if _, err := parseAttachResponse([]byte(attach(`,"recording":{"id":"nope","trigger":"rule","started_at":"2026-11-11T00:00:00Z"}`))); err == nil {
		t.Fatal("an invalid recording was accepted")
	}
}

func TestRecordingFailureMessagesAreAuthored(t *testing.T) {
	for _, code := range []string{wire.ErrorUnavailable, wire.ErrorForbidden, wire.ErrorActive, wire.ErrorInactive, wire.ErrorNotFound} {
		if got, message := browserRecordingFailure(code); got != code || message == "" {
			t.Fatalf("%s -> %q %q", code, got, message)
		}
	}
	if code, _ := browserRecordingFailure("secret prose from the machine"); code != "recording_failed" {
		t.Fatalf("unknown code passed through: %q", code)
	}
}

// The worker's paced replay shows the page one stream: offsets continue across ranges, the size is
// fixed by the first answer, and an answer that skips, repeats or stalls is refused.
func TestPacedReplayIsOneStream(t *testing.T) {
	const id = "rec_00000000-0000-4000-8000-000000000001"
	r := replayRange{recordingID: id}
	if !r.bind(replayReadResult{RecordingID: id, StreamID: 3, FromSegment: 0, Segments: 1, TotalSegments: 2, Bytes: 100, TotalBytes: 150}, true) || r.streamBytes() != 158 {
		t.Fatalf("first range %+v", r)
	}
	if from, more := r.ended(); from != 1 || !more || r.base != 104 {
		t.Fatalf("after the first range from=%d more=%v base=%d", from, more, r.base)
	}
	for name, bad := range map[string]replayReadResult{
		"repeated":  {RecordingID: id, StreamID: 4, FromSegment: 0, Segments: 1, Bytes: 100},
		"stalled":   {RecordingID: id, StreamID: 4, FromSegment: 1, Segments: 0},
		"other":     {RecordingID: "rec_00000000-0000-4000-8000-000000000002", StreamID: 4, FromSegment: 1, Segments: 1, Bytes: 50},
		"no_stream": {RecordingID: id, FromSegment: 1, Segments: 1, Bytes: 50},
	} {
		if copy := r; copy.bind(bad, false) {
			t.Fatalf("%s range accepted", name)
		}
	}
	if !r.bind(replayReadResult{RecordingID: id, StreamID: 4, FromSegment: 1, Segments: 1, Bytes: 50, Final: true}, false) || !r.final {
		t.Fatalf("second range %+v", r)
	}
	if from, more := r.ended(); from != 2 || more || r.base != 158 {
		t.Fatalf("after the last range from=%d more=%v base=%d", from, more, r.base)
	}
	empty := replayRange{recordingID: id}
	if !empty.bind(replayReadResult{RecordingID: id, StreamID: 1, TotalSegments: 0}, true) {
		t.Fatal("an empty recording was refused")
	}
	if _, more := empty.ended(); more {
		t.Fatal("an empty recording asked for more")
	}
}
