package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/command"
	"github.com/pinksaucepasta/paperboat/internal/tunnel"
)

func TestExecStartedEventShowsOnePendingEnvironmentLine(t *testing.T) {
	event := tunnel.ExecEvent{OperationID: "operation_1", State: "started", Warnings: []string{"environment_pending"}}
	var out, errOut bytes.Buffer
	ctx := &command.Context{Writer: &out, ErrWriter: &errOut}
	if err := writeExecEvent(ctx, nil, event, false, false); err != nil {
		t.Fatal(err)
	}
	if strings.Count(errOut.String(), "\n") != 1 || !strings.Contains(errOut.String(), "pb env vault unlock") || out.Len() != 0 {
		t.Fatalf("stderr=%q stdout=%q", errOut.String(), out.String())
	}
	out.Reset()
	errOut.Reset()
	if err := writeExecEvent(ctx, json.NewEncoder(&out), event, false, true); err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Event    string          `json:"event"`
		Warnings []launchWarning `json:"warnings"`
	}
	if err := json.Unmarshal(out.Bytes(), &wire); err != nil || wire.Event != "started" || len(wire.Warnings) != 1 || wire.Warnings[0].Code != "environment_pending" || errOut.Len() != 0 {
		t.Fatalf("json=%s stderr=%q err=%v", out.String(), errOut.String(), err)
	}
}
