package diagnostics

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
)

func TestReconnectIPCTracePreservesCorrelationWithoutPrivateErrors(t *testing.T) {
	recorder := NewMemoryRecorder()
	cli := WithReconnect(t.Context(), "attempt-1", "probeStalled", "connection-1")
	h := http.Header{}
	ReconnectHeaders(cli, h)
	daemon := ReconnectRequest(WithRecorder(context.Background(), recorder), h)
	end := ReconnectStep(daemon, "attach_ack")
	end(errors.New("PRIVATE token and terminal contents"))
	events := recorder.Recent()
	if len(events) != 2 {
		t.Fatalf("events=%d", len(events))
	}
	for _, event := range events {
		f := event.Fields
		if event.Validate() != nil || f["attempt_id"] != "attempt-1" || f["caller"] != "probeStalled" || f["connection_id"] != "connection-1" || f["step_id"] != events[0].Fields["step_id"] {
			t.Fatal("IPC lost correlation")
		}
	}
	encoded, _ := json.Marshal(events)
	if strings.Contains(string(encoded), "PRIVATE") || events[1].Fields["error_type"] != "errorString" || events[1].Fields["state"] != "failed" {
		t.Fatal("fault projection leaked or lost typed failure")
	}
	for _, bad := range []string{"attempt:supervise:connection:extra", "attempt:other:connection", "secret value:supervise:connection"} {
		h.Set(ReconnectHeader, bad)
		ctx := ReconnectRequest(WithRecorder(context.Background(), recorder), h)
		ReconnectEvent(ctx, "redial", "started", nil)
	}
	if len(recorder.Recent()) != 2 {
		t.Fatal("untrusted IPC metadata accepted")
	}
}
