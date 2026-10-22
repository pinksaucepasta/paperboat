package main

import (
	"encoding/json"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/protocol"
)

const (
	connectionModeTerminal  = "terminal"
	connectionModeInspector = "inspector"

	inspectorCapability  = "inspector.v1"
	inspectorDeadlineMS  = 20000
	inspectorMaxInFlight = 4
	inspectorRequestMax  = 64 << 10
)

// helloPayload offers the capabilities a connection mode uses. terminal.v1
// and health.v1 are required by every daemon handshake.
func helloPayload(mode string) json.RawMessage {
	if mode == connectionModeInspector {
		return json.RawMessage(`{"min_version":"1.0","max_version":"1.0","capabilities":["terminal.v1","health.v1","inspector.v1"]}`)
	}
	return json.RawMessage(`{"min_version":"1.0","max_version":"1.0","capabilities":["terminal.v1","health.v1","file-transfer.v1"]}`)
}

// inspectorRequestFrame builds one inspector.v1 request within the request
// frame limit.
func inspectorRequestFrame(requestID, operationID string, payload json.RawMessage) (protocol.Frame, []byte, bool) {
	frame := protocol.Frame{Type: "request", RequestID: requestID, Version: protocol.ProtocolVersion, OperationID: operationID, Capability: inspectorCapability, DeadlineMS: inspectorDeadlineMS, Payload: payload}
	encoded, err := json.Marshal(frame)
	if err != nil || len(payload) > inspectorRequestMax || frame.Validate() != nil || len(encoded) > protocol.MaxStructuredFrame {
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
