//go:build js && wasm

package main

import (
	"encoding/json"
	"syscall/js"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/protocol"
	wire "github.com/pinksaucepasta/paperboat/shared/recording"
)

// apiRecordingRead(connectionID, recordingID) replays one recording on a recording-mode connection. It
// resolves {result:{recording_id, segments, bytes, final}} (bytes: what the output events will carry
// in all); the recording then arrives as "output" events (start_sequence = byte offset, data = each
// segment file behind a 4-byte big-endian length) and one recording_stream_end "event". The worker
// reads one segment per request underneath, so the receive queue stays bounded.
func apiRecordingRead(_ js.Value, args []js.Value) any {
	if len(args) != 2 || args[1].Type() != js.TypeString || !wire.ValidRecordingID(args[1].String()) {
		return errorObject("invalid_request", "The recording request is invalid.")
	}
	connection := getConnection(args[0].String())
	if connection == nil || connection.config.Mode != connectionModeRecording {
		return errorObject("connection_closed", "The recording connection is closed. Reconnect to continue.")
	}
	recordingID := args[1].String()
	return browserPromise(func() any {
		connection.mu.Lock()
		if connection.replay != nil {
			connection.mu.Unlock()
			return errorObject("request_busy", "This connection is already replaying a recording.")
		}
		connection.replay = &replayRange{recordingID: recordingID}
		connection.mu.Unlock()
		if failed := connection.readReplayRange(0, true); failed != nil {
			return failed
		}
		connection.mu.Lock()
		replay := *connection.replay
		connection.mu.Unlock()
		return jsObject(map[string]any{"result": jsObject(map[string]any{"recording_id": recordingID, "segments": replay.total, "bytes": float64(replay.streamBytes()), "final": replay.final})})
	})
}

// readReplayRange asks for one segment from `from`; the answer binds the stream in the read loop,
// before the segment's frames arrive.
func (connection *browserTerminal) readReplayRange(from uint32, first bool) any {
	connection.mu.Lock()
	recordingID := connection.replay.recordingID
	connection.mu.Unlock()
	payload, _ := json.Marshal(map[string]any{"action": wire.ReadAction, "recording_id": recordingID, "from_segment": from, "max_segments": 1})
	bound := false
	_, failed := connection.callFrame(wire.Capability, payload, connection.recordingReady, browserRecordingFailure, func(response protocol.Frame) {
		var body struct {
			Result replayReadResult `json:"result"`
		}
		if response.Type != "response" || json.Unmarshal(response.Payload, &body) != nil {
			return
		}
		connection.mu.Lock()
		if bound = connection.replay != nil && connection.replay.bind(body.Result, first); bound {
			connection.streamID = body.Result.StreamID
		}
		connection.mu.Unlock()
	})
	if failed != nil {
		return failed
	}
	if !bound {
		return errorObject("invalid_response", "The machine returned an invalid recording answer.")
	}
	return nil
}

// replayOffset is an output frame's offset in the page's one replay stream.
func (connection *browserTerminal) replayOffset(start uint64) uint64 {
	connection.mu.Lock()
	defer connection.mu.Unlock()
	if connection.replay == nil {
		return start
	}
	return connection.replay.base + start
}

// replayContinues runs in the read loop on recording_stream_end: while segments remain it asks for the
// next one and swallows the event; the last range's event reaches the page.
func (connection *browserTerminal) replayContinues() bool {
	connection.mu.Lock()
	if connection.replay == nil {
		connection.mu.Unlock()
		return false
	}
	from, more := connection.replay.ended()
	connection.mu.Unlock()
	if !more {
		return false
	}
	go func() {
		if failed := connection.readReplayRange(from, false); failed != nil {
			value := failed.(js.Value)
			connection.emit("state", map[string]any{"state": "transport-error", "code": value.Get("error").String(), "message": value.Get("message").String()})
		}
	}()
	return true
}
