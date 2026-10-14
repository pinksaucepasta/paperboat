package usermachines

import (
	"context"
	"strings"
	"syscall"
	"testing"
)

type errorMessageProbe struct{ calls int }

func (e *errorMessageProbe) Error() string {
	e.calls++
	return "secret bearer token must not be stored"
}

func (*errorMessageProbe) Unwrap() error                  { return syscall.ECONNREFUSED }
func (*errorMessageProbe) DiagnosticProtocolCode() string { return "frame_invalid" }

func TestTerminalErrorSummaryPersistsOnlySafeCauseMetadata(t *testing.T) {
	err := &errorMessageProbe{}
	summary := terminalErrorSummary(context.Background(), err)
	if err.calls != 0 {
		t.Fatalf("error message formatted %d times", err.calls)
	}
	if !strings.Contains(summary, "cause=connection_refused") || !strings.Contains(summary, "error_type=") || !strings.Contains(summary, "protocol_code=frame_invalid") || strings.Contains(summary, "secret bearer token") {
		t.Fatalf("unexpected terminal error summary: %q", summary)
	}
}
