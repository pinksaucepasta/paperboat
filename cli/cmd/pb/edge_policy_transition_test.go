package main

import (
	"testing"

	"github.com/pinksaucepasta/paperboat/cli/internal/api"
)

func TestEdgePolicyTransitionRequiredSaysHowToLimitViewers(t *testing.T) {
	const want = "A public preview takes a list of emails or domains as it runs; to make it private or team-only, publish it again."
	err := &api.APIError{Status: 409, Code: "edge_policy_transition_required"}
	if got := friendlyAPIError(err); got != want {
		t.Fatalf("friendlyAPIError = %q, want %q", got, want)
	}
	if message := commandFailureMessage(err, classifyCommandFailure(err)); message != want {
		t.Fatalf("commandFailureMessage = %q, want %q", message, want)
	}
}
