package main

import (
	"encoding/json"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/protocol"
	wire "github.com/pinksaucepasta/paperboat/shared/recording"
)

const (
	connectionModeTerminal  = "terminal"
	connectionModeInspector = "inspector"
	connectionModeRecording = "recording" // replays one recording: recording.v1 reads, no terminal attach

	inspectorCapability = "inspector.v1"
	callDeadlineMS      = 20000
	callMaxInFlight     = 4
	callRequestMax      = 64 << 10
)

// helloPayload offers the capabilities a connection mode uses. terminal.v1
// and health.v1 are required by every daemon handshake.
func helloPayload(mode string) json.RawMessage {
	switch mode {
	case connectionModeInspector:
		return json.RawMessage(`{"min_version":"1.0","max_version":"1.0","capabilities":["terminal.v1","health.v1","inspector.v1"]}`)
	case connectionModeRecording:
		return json.RawMessage(`{"min_version":"1.0","max_version":"1.0","capabilities":["terminal.v1","health.v1","recording.v1"]}`)
	}
	return json.RawMessage(`{"min_version":"1.0","max_version":"1.0","capabilities":["terminal.v1","health.v1","file-transfer.v1"]}`)
}

// callRequestFrame builds one request (inspector.v1, recording.v1 or a
// terminal.v1 recording control) within the request frame limit.
func callRequestFrame(requestID, operationID, capability string, payload json.RawMessage) (protocol.Frame, []byte, bool) {
	frame := protocol.Frame{Type: "request", RequestID: requestID, Version: protocol.ProtocolVersion, OperationID: operationID, Capability: capability, DeadlineMS: callDeadlineMS, Payload: payload}
	encoded, err := json.Marshal(frame)
	if err != nil || len(payload) > callRequestMax || frame.Validate() != nil || len(encoded) > protocol.MaxStructuredFrame {
		return protocol.Frame{}, nil, false
	}
	return frame, encoded, true
}

// Only authored codes reach the console; remote prose never does.
func browserInspectorFailure(remoteCode string) (string, string) {
	switch remoteCode {
	case "capability_required", "capability_disabled":
		return "capability_required", "Update Paperboat on this machine to inspect previews from the console."
	case "not_found_or_forbidden":
		return remoteCode, "You do not have access to inspect this preview."
	case "credential_expired":
		return remoteCode, "Inspector access expired. Reconnecting."
	case "invalid_request":
		return remoteCode, "The inspector request is invalid."
	case "response_too_large":
		return remoteCode, "This capture is too large to show in the console."
	case "operation_canceled", "deadline_exceeded", "unavailable", "resource_limit":
		return remoteCode, "The machine could not answer the inspector request. Retry."
	default:
		return "inspector_failed", "The inspector request could not be completed."
	}
}

// Only authored codes reach the console; remote prose never does.
func browserRecordingFailure(remoteCode string) (string, string) {
	switch remoteCode {
	case wire.ErrorUnavailable:
		return remoteCode, "Recording is not available for this session: the team needs the Teams plan and a recording key."
	case wire.ErrorForbidden:
		return remoteCode, "You may not do that: only admins may start recordings here, or a team rule started this one."
	case wire.ErrorActive:
		return remoteCode, "This session is already being recorded."
	case wire.ErrorInactive:
		return remoteCode, "This session is not being recorded."
	case wire.ErrorNotFound:
		return remoteCode, "The machine no longer has this recording."
	case "not_found_or_forbidden":
		return remoteCode, "You do not have access to this recording."
	case "capability_required", "capability_disabled":
		return "capability_required", "Update Paperboat on this machine to use recordings."
	case "credential_expired":
		return remoteCode, "Replay access expired. Open the recording again."
	case "operation_canceled", "deadline_exceeded", "unavailable", "resource_limit":
		return remoteCode, "The machine could not answer. Retry."
	default:
		return "recording_failed", "The recording request could not be completed."
	}
}
