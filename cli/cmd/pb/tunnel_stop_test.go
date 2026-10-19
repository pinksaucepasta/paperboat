package main

import (
	"strings"
	"testing"
)

// `pb tunnel stop <preview>` used to demand --ephemeral and fail with a generic local error.
func TestTunnelStopPointsDurableTunnelsToPauseOrDelete(t *testing.T) {
	command := tunnelCobraCommandV1()
	command.SetArgs([]string{"stop", "tunnel_0123"})
	err := command.Execute()
	failure := classifyCommandFailure(err)
	if err == nil || failure.kind != commandUsage || !strings.Contains(err.Error(), "pb tunnel pause") {
		t.Fatalf("kind=%v err=%v", failure.kind, err)
	}
}
