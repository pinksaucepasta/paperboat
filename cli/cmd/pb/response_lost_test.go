package main

import (
	"fmt"
	"net"
	"net/url"
	"strings"
	"syscall"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/api"
)

// A team action the server applied but whose answer was lost (Lane S, support_5ed29c4f) says it may
// have been applied and is outcome_uncertain, not "unreachable".
func TestALostResponseSaysTheChangeMayHaveBeenApplied(t *testing.T) {
	err := fmt.Errorf("team action: %w", &api.ResponseLostError{Method: "POST", Path: "/v1/teams/t/actions", Err: &url.Error{Op: "Post", URL: "https://api.example.test/v1/teams/t/actions", Err: &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}}})
	failure := classifyCommandFailure(err)
	message := commandFailureMessage(err, failure)
	result := classifyCLIJSONError(err)
	if strings.Contains(message, "unreachable") || !strings.Contains(message, "may have been applied") || !result.OutcomeUncertain || result.Code != "response_lost" || result.StateChanged != "unknown" {
		t.Fatalf("message=%q result=%+v", message, result)
	}
}
