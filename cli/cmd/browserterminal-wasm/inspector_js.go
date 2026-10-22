//go:build js && wasm

package main

import (
	"encoding/json"
	"strings"
	"syscall/js"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/protocol"
)

// apiInspectorRequest(connectionID, payload) sends one inspector.v1 call
// over an inspector-mode connection and resolves {result} or {error, message}.
func apiInspectorRequest(_ js.Value, args []js.Value) any {
	if len(args) != 2 || args[1].Type() != js.TypeObject {
		return errorObject("invalid_request", "The inspector request is invalid.")
	}
	connection := getConnection(args[0].String())
	if connection == nil || connection.config.Mode != connectionModeInspector {
		return errorObject("connection_closed", "The inspector connection is closed. Reconnect to continue.")
	}
	encoded := js.Global().Get("JSON").Call("stringify", args[1])
	if encoded.Type() != js.TypeString || len(encoded.String()) > inspectorRequestMax {
		return errorObject("request_limit", "The inspector request is too large.")
	}
	payload := json.RawMessage(encoded.String())
	return browserPromise(func() any { return connection.inspectorRequest(payload) })
}

func (connection *browserTerminal) inspectorRequest(payload json.RawMessage) any {
	requestID, err := randomID("inspector_request")
	if err != nil {
		return errorObject("inspector_failed", "The inspector request could not be created.")
	}
	operationID, err := randomID("operation")
	if err != nil {
		return errorObject("inspector_failed", "The inspector request could not be created.")
	}
	_, encoded, ok := inspectorRequestFrame(requestID, operationID, payload)
	if !ok {
		return errorObject("invalid_request", "The inspector request is invalid.")
	}
	reply := make(chan protocol.Frame, 1)
	connection.mu.Lock()
	if !connection.attached || connection.stopping.Load() {
		connection.mu.Unlock()
		return errorObject("connection_closed", "The inspector connection is closed. Reconnect to continue.")
	}
	if !connection.inspectorReady {
		connection.mu.Unlock()
		code, message := browserInspectorFailure("capability_required")
		return errorObject(code, message)
	}
	if len(connection.inspectorCalls) >= inspectorMaxInFlight {
		connection.mu.Unlock()
		return errorObject("inspector_busy", "Too many inspector requests are running. Retry shortly.")
	}
	connection.inspectorCalls[requestID] = reply
	err = connection.writes.enqueue(terminalWrite{kind: appKindStructured, payloads: [][]byte{encoded}, bytes: len(encoded) + 5})
	connection.mu.Unlock()
	defer func() { connection.mu.Lock(); delete(connection.inspectorCalls, requestID); connection.mu.Unlock() }()
	if err != nil {
		return errorObject("inspector_busy", "The encrypted connection is busy. Retry shortly.")
	}
	timer := time.NewTimer(inspectorDeadlineMS * time.Millisecond)
	defer timer.Stop()
	select {
	case response := <-reply:
		if response.Type == "error" {
			var body struct {
				Code string `json:"code"`
			}
			if json.Unmarshal(response.Payload, &body) != nil {
				return errorObject("invalid_response", "The machine returned an invalid inspector response.")
			}
			code, message := browserInspectorFailure(body.Code)
			if body.Code == "credential_expired" {
				connection.emit("state", map[string]any{"state": "reconnecting", "code": body.Code, "message": message})
			}
			return errorObject(code, message)
		}
		return js.Global().Get("JSON").Call("parse", string(response.Payload))
	case <-connection.fileDone:
		return errorObject("connection_closed", "The inspector connection is closed. Reconnect to continue.")
	case <-timer.C:
		return errorObject("inspector_timeout", "The machine did not answer in time. Retry.")
	}
}

func (connection *browserTerminal) deliverInspectorResponse(frame protocol.Frame) bool {
	if !strings.HasPrefix(frame.RequestID, "inspector_request_") || frame.Type != "response" && frame.Type != "error" {
		return false
	}
	connection.mu.Lock()
	pending := connection.inspectorCalls[frame.RequestID]
	connection.mu.Unlock()
	if pending != nil {
		select {
		case pending <- frame:
		default:
		}
	}
	return true
}
