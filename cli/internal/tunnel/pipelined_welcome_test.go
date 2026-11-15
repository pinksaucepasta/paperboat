package tunnel

import (
	"encoding/json"
	"errors"
	"io"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/peertransport/tailnet"
	"github.com/pinksaucepasta/paperboat/internal/resolver"
)

// pipelinedExec sends hello and starts an exec over a pipelined welcome; first answers in place of the welcome.
// It checks hello and the start request both leave before anything is read, and returns initialize's error.
func pipelinedExec(t *testing.T, first func(helloID string) execMessage, thenResponse bool) error {
	t.Helper()
	message := newExecMessageConnection()
	helloID, err := writeNativeHello(t.Context(), message)
	if err != nil {
		t.Fatal(err)
	}
	request := ExecRequest{OperationID: "operation_exec_1", Argv: []string{"/bin/true"}}
	exec := &helperExecConn{message: &pipelinedWelcome{helperMessageConnection: message, helloID: helloID}, target: &resolver.TerminalTarget{}, request: request, events: make(chan ExecEvent, 4), done: make(chan struct{}), pending: make(map[string]chan helperFrame)}
	go func() {
		var hello, start helperFrame
		_ = json.Unmarshal((<-message.writes).data, &hello)
		_ = json.Unmarshal((<-message.writes).data, &start) // sent before any answer: one flight
		if hello.Type != "hello" || start.Type != "request" || start.Capability != "exec.v1" {
			message.reads <- execMessage{err: errors.New("frames out of order")}
			return
		}
		message.reads <- first(helloID)
		if thenResponse {
			payload, _ := json.Marshal(map[string]any{"result": map[string]any{"stream_id": 1, "snapshot": map[string]any{"operation_id": request.OperationID, "state": "running"}}})
			response, _ := json.Marshal(helperFrame{Type: "response", RequestID: start.RequestID, Version: helperProtocolVersion, OperationID: request.OperationID, Payload: payload})
			message.reads <- execMessage{kind: helperStructuredMessage, data: response}
		}
	}()
	err = exec.initialize(t.Context())
	if err == nil {
		_ = exec.Detach()
	}
	return err
}

func welcomeFrame(id string) execMessage {
	payload, _ := json.Marshal(map[string]any{"version": helperProtocolVersion, "capabilities": []string{"terminal.v1", "health.v1", "exec.v1"}, "binding_secret": make([]byte, nativeBindingSize)})
	data, _ := json.Marshal(helperFrame{Type: "welcome", RequestID: id, Version: helperProtocolVersion, Payload: payload})
	return execMessage{kind: helperStructuredMessage, data: data}
}

func TestPipelinedExecStartsInOneFlight(t *testing.T) {
	if err := pipelinedExec(t, welcomeFrame, true); err != nil {
		t.Fatalf("pipelined exec start: %v", err)
	}
}

// A refused stream (admission reset or an error in reply to hello) never ran the request: not started, never uncertain.
func TestPipelinedExecRefusalIsNotStarted(t *testing.T) {
	cases := map[string]func(string) execMessage{
		"admission": func(string) execMessage {
			return execMessage{err: errors.Join(tailnet.ErrAdmission, errors.New("denied"))}
		},
		"hello": func(id string) execMessage {
			data, _ := json.Marshal(helperFrame{Type: "error", RequestID: id, Version: helperProtocolVersion, Payload: json.RawMessage(`{"code":"protocol_incompatible"}`)})
			return execMessage{kind: helperStructuredMessage, data: data}
		},
	}
	for name, first := range cases {
		err := pipelinedExec(t, first, false)
		var refused *errNotAdmitted
		if !errors.As(err, &refused) {
			t.Fatalf("%s refusal: %v, want errNotAdmitted (DialExec reports not started)", name, err)
		}
	}
}

// Anything else in place of the welcome fails closed, and since the start request may have reached the host it is
// uncertain, never "not started": a lost transport, or the start response arriving before the welcome.
func TestPipelinedExecOutOfOrderOrLostWelcomeIsUncertain(t *testing.T) {
	cases := map[string]func(string) execMessage{
		"lost": func(string) execMessage { return execMessage{err: io.ErrUnexpectedEOF} },
		"response first": func(id string) execMessage {
			data, _ := json.Marshal(helperFrame{Type: "response", RequestID: id, Version: helperProtocolVersion})
			return execMessage{kind: helperStructuredMessage, data: data}
		},
	}
	for name, first := range cases {
		err := pipelinedExec(t, first, true)
		var refused *errNotAdmitted
		var uncertain *ExecStartUncertainError
		if err == nil || errors.As(err, &refused) || !errors.As(err, &uncertain) {
			t.Fatalf("%s: %v, want an uncertain start failure", name, err)
		}
	}
}
