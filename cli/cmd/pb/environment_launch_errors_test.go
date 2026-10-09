package main

import (
	"github.com/pinksaucepasta/paperboat/internal/localapi"
	"strings"
	"testing"
)

func TestEnvironmentLaunchDenialPresentationIsActionableAndPrivate(t *testing.T) {
	err := &localapi.RemoteError{StatusCode: 503, Code: "environment_unavailable", Message: "private ENV value"}
	for _, message := range []string{commandFailureMessage(err, classifyCommandFailure(err)), safeExecError(err)} {
		if !strings.Contains(message, "ENV delivery") || strings.Contains(message, "private ENV") {
			t.Fatalf("message=%q", message)
		}
	}
	if publicExecErrorCode("environment_unavailable") != "environment_unavailable" {
		t.Fatal("machine code discarded")
	}
}
