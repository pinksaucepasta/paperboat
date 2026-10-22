package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/protocol"
)

func TestInspectorHelloOffersInspectorOnlyInInspectorMode(t *testing.T) {
	var hello struct {
		Capabilities []string `json:"capabilities"`
	}
	if json.Unmarshal(helloPayload(connectionModeInspector), &hello) != nil || strings.Join(hello.Capabilities, ",") != "terminal.v1,health.v1,inspector.v1" {
		t.Fatalf("inspector hello=%v", hello.Capabilities)
	}
	if json.Unmarshal(helloPayload(connectionModeTerminal), &hello) != nil || strings.Join(hello.Capabilities, ",") != "terminal.v1,health.v1,file-transfer.v1" {
		t.Fatalf("terminal hello=%v", hello.Capabilities)
	}
}

func TestInspectorRequestFrameIsBounded(t *testing.T) {
	frame, encoded, ok := inspectorRequestFrame("inspector_request_1", "operation_12345678", json.RawMessage(`{"grant":"g","method":"GET","path":"/v1/inspector/records"}`))
	if !ok || frame.Capability != "inspector.v1" || frame.DeadlineMS != 20000 || len(encoded) == 0 {
		t.Fatalf("frame=%+v ok=%v", frame, ok)
	}
	large := json.RawMessage(`{"body":"` + strings.Repeat("a", 64<<10) + `"}`)
	if _, _, ok := inspectorRequestFrame("inspector_request_2", "operation_12345678", large); ok {
		t.Fatal("oversized inspector request accepted")
	}
}

func TestInspectorFailureMessagesAreAuthored(t *testing.T) {
	if code, message := browserInspectorFailure("capability_required"); code != "capability_required" || message != "Update Paperboat on this machine to inspect previews from the console." {
		t.Fatalf("capability_required -> %q %q", code, message)
	}
	if code, _ := browserInspectorFailure("secret prose from the machine"); code != "inspector_failed" {
		t.Fatalf("unknown code passed through: %q", code)
	}
}

func TestLargeStructuredFramesMustBeResponses(t *testing.T) {
	big := json.RawMessage(`{"result":{"body":"` + strings.Repeat("a", 100<<10) + `"}}`)
	for _, kind := range []string{"response", "event"} {
		encoded, err := json.Marshal(protocol.Frame{Type: kind, RequestID: "inspector_request_1", Version: protocol.ProtocolVersion, Payload: big})
		if err != nil {
			t.Fatal(err)
		}
		var wire bytes.Buffer
		wire.WriteByte(appKindStructured)
		_ = binary.Write(&wire, binary.BigEndian, uint32(len(encoded)))
		wire.Write(encoded)
		_, err = readStructuredFrame(&wire)
		if (err == nil) != (kind == "response") {
			t.Fatalf("%s frame of %d bytes err=%v", kind, len(encoded), err)
		}
	}
}
