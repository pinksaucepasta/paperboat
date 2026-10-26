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
	ctx := &command.Context{Reader: strings.NewReader(""), Writer: &out, ErrWriter: &errOut}
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

func TestUndeliverableEnvironmentLaunchPointsAtHostShowNotVaultUnlock(t *testing.T) {
	pending := launchWarningLine([]string{"environment_pending"})
	failed := launchWarningLine([]string{"environment_undeliverable"})
	if !strings.Contains(pending, "pending") || !strings.Contains(pending, "pb env vault unlock") {
		t.Fatalf("pending line=%q", pending)
	}
	if !strings.Contains(failed, "failed") || !strings.Contains(failed, "pb env host show") || strings.Contains(failed, "pending") {
		t.Fatalf("undeliverable line=%q", failed)
	}
}
