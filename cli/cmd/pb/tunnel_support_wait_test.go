package main

import (
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/spf13/cobra"
)

// A change the server completes while answering (removing a domain) returns the resource and no
// operation; the command must report it as done, not as an unsafe response.
func TestWaitForTunnelOperationAcceptsAResourceOnlyAnswer(t *testing.T) {
	for _, wait := range []bool{false, true} {
		command := &cobra.Command{Use: "remove"}
		tunnelMutationWaitFlags(command)
		if wait {
			_ = command.Flags().Set("wait", "true")
		}
		got, err := waitForTunnelOperation(command, nil, api.TunnelOperation{})
		if err != nil || got.Schema != "" {
			t.Fatalf("wait=%v operation=%#v error=%v", wait, got, err)
		}
		if human := tunnelOperationHuman(got); human != "ready" {
			t.Fatalf("wait=%v human=%q", wait, human)
		}
	}
}

func TestWaitForTunnelOperationStillRejectsUnknownStates(t *testing.T) {
	command := &cobra.Command{Use: "remove"}
	tunnelMutationWaitFlags(command)
	if _, err := waitForTunnelOperation(command, nil, api.TunnelOperation{Schema: api.TunnelV1Schema, State: "unknown"}); err == nil {
		t.Fatal("an operation in an unknown state was accepted")
	}
}
