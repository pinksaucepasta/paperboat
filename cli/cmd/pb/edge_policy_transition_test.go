package main

import (
	"testing"

	"github.com/pinksaucepasta/paperboat/cli/internal/api"
)

func TestEdgePolicyTransitionRequiredSaysHowToLimitViewers(t *testing.T) {
	const want = "To limit who can open this preview, publish it again with --allow <emails/domains>."
	err := &api.APIError{Status: 409, Code: "edge_policy_transition_required"}
	if got := friendlyAPIError(err); got != want {
		t.Fatalf("friendlyAPIError = %q, want %q", got, want)
	}
	if message := commandFailureMessage(err, classifyCommandFailure(err)); message != want {
		t.Fatalf("commandFailureMessage = %q, want %q", message, want)
	}
}
