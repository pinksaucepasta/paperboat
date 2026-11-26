package main

import (
	"strings"
	"testing"
)

// pb send with host-local delivery settings but no terminal session must name the step, not "could not finish".
func TestHostLocalSendWithoutSessionNamesStep(t *testing.T) {
	err := error(commandRejection{reason: commandRejectHostLocalSendNoSession})
	text := commandFailureMessage(err, classifyCommandFailure(err))
	if !strings.Contains(text, "host-local delivery") || !strings.Contains(text, "--session") || strings.Contains(text, "could not finish") {
		t.Fatalf("message = %q", text)
	}
}
