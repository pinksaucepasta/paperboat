package controlplane

import "testing"

// A relay starts unless its node is retired or draining; its registered size is not a condition (there is no node limit).
func TestRelayNodeStartDependsOnlyOnState(t *testing.T) {
	for state, want := range map[string]bool{"ready": true, "registered": true, "offline": true, "draining": false, "retired": false} {
		if got := relayNodeMayStart(state); got != want {
			t.Errorf("state %s may start = %v, want %v", state, got, want)
		}
	}
}
