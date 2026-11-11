//go:build js && wasm

package main

import (
	"encoding/json"
	"strings"
	"syscall/js"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/protocol"
	wire "github.com/pinksaucepasta/paperboat/shared/recording"
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
	if encoded.Type() != js.TypeString || len(encoded.String()) > callRequestMax {
		return errorObject("request_limit", "The inspector request is too large.")
	}
	payload := json.RawMessage(encoded.String())
	return browserPromise(func() any {
		return connection.call(inspectorCapability, payload, connection.inspectorReady, browserInspectorFailure, nil)
	})
}

// apiRecordingControl(connectionID, action) starts or stops the recording of the attached session
// ("record-start" | "record-stop") as this attachment's account, for any role: the machine decides.
func apiRecordingControl(_ js.Value, args []js.Value) any {
	if len(args) != 2 || args[1].Type() != js.TypeString || args[1].String() != wire.ActionStart && args[1].String() != wire.ActionStop {
		return errorObject("invalid_request", "The recording request is invalid.")
	}
	connection := getConnection(args[0].String())
	if connection == nil || connection.config.Mode != connectionModeTerminal {
		return errorObject("connection_closed", "The terminal connection is closed. Reconnect to continue.")
	}
	payload, _ := json.Marshal(map[string]string{"action": args[1].String(), "session_id": connection.config.TerminalSessionID})
	return browserPromise(func() any {
		return connection.call("terminal.v1", payload, true, browserRecordingFailure, nil)
	})
}

type pendingCall struct {
	reply      chan protocol.Frame
	onResponse func(protocol.Frame) // runs in the read loop, before the next frame
}

// call sends one request on this connection and resolves the response payload or {error, message}.
func (connection *browserTerminal) call(capability string, payload json.RawMessage, ready bool, failure func(string) (string, string), onResponse func(protocol.Frame)) any {
	response, failed := connection.callFrame(capability, payload, ready, failure, onResponse)
	if failed != nil {
		return failed
	}
	return js.Global().Get("JSON").Call("parse", string(response.Payload))
}

// callFrame sends one request and returns its response frame, or the {error, message} to resolve.
func (connection *browserTerminal) callFrame(capability string, payload json.RawMessage, ready bool, failure func(string) (string, string), onResponse func(protocol.Frame)) (protocol.Frame, any) {
	requestID, err := randomID("call_request")
	if err != nil {
		return protocol.Frame{}, errorObject("request_failed", "The request could not be created.")
	}
	operationID, err := randomID("operation")
	if err != nil {
		return protocol.Frame{}, errorObject("request_failed", "The request could not be created.")
	}
	_, encoded, ok := callRequestFrame(requestID, operationID, capability, payload)
	if !ok {
		return protocol.Frame{}, errorObject("invalid_request", "The request is invalid.")
	}
	pending := pendingCall{reply: make(chan protocol.Frame, 1), onResponse: onResponse}
	connection.mu.Lock()
	if !connection.attached || connection.stopping.Load() {
		connection.mu.Unlock()
		return protocol.Frame{}, errorObject("connection_closed", "The connection is closed. Reconnect to continue.")
	}
	if !ready {
		connection.mu.Unlock()
		code, message := failure("capability_required")
		return protocol.Frame{}, errorObject(code, message)
	}
	if len(connection.calls) >= callMaxInFlight {
		connection.mu.Unlock()
		return protocol.Frame{}, errorObject("request_busy", "Too many requests are running. Retry shortly.")
	}
	connection.calls[requestID] = pending
	err = connection.writes.enqueue(terminalWrite{kind: appKindStructured, payloads: [][]byte{encoded}, bytes: len(encoded) + 5})
	connection.mu.Unlock()
	defer func() { connection.mu.Lock(); delete(connection.calls, requestID); connection.mu.Unlock() }()
	if err != nil {
		return protocol.Frame{}, errorObject("request_busy", "The encrypted connection is busy. Retry shortly.")
	}
	timer := time.NewTimer(callDeadlineMS * time.Millisecond)
	defer timer.Stop()
	select {
	case response := <-pending.reply:
		if response.Type == "error" {
			var body struct {
				Code string `json:"code"`
			}
			if json.Unmarshal(response.Payload, &body) != nil {
				return protocol.Frame{}, errorObject("invalid_response", "The machine returned an invalid response.")
			}
			code, message := failure(body.Code)
			if body.Code == "credential_expired" && capability == inspectorCapability {
				connection.emit("state", map[string]any{"state": "reconnecting", "code": body.Code, "message": message})
			}
			return protocol.Frame{}, errorObject(code, message)
		}
		return response, nil
	case <-connection.fileDone:
		return protocol.Frame{}, errorObject("connection_closed", "The connection is closed. Reconnect to continue.")
	case <-timer.C:
		return protocol.Frame{}, errorObject("request_timeout", "The machine did not answer in time. Retry.")
	}
}

func (connection *browserTerminal) deliverCallResponse(frame protocol.Frame) bool {
	if !strings.HasPrefix(frame.RequestID, "call_request_") || frame.Type != "response" && frame.Type != "error" {
		return false
	}
	connection.mu.Lock()
	pending, ok := connection.calls[frame.RequestID]
	connection.mu.Unlock()
	if ok {
		if pending.onResponse != nil {
			pending.onResponse(frame)
		}
		select {
		case pending.reply <- frame:
		default:
		}
	}
	return true
}
