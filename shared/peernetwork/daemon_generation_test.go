package peernetwork

import "testing"

func TestDaemonEndpointID(t *testing.T) {
	const generation = "0123456789abcdef0123456789abcdef"
	if got, ok := DaemonEndpointID("cli_abc", generation); !ok || got != "cli_abc_"+generation {
		t.Fatalf("DaemonEndpointID = %q, %v", got, ok)
	}
	for _, bad := range []string{"", "0123456789ABCDEF0123456789abcdef", generation[:31], generation + "0", "0123456789abcdef0123456789abcdeg"} {
		if _, ok := DaemonEndpointID("cli_abc", bad); ok {
			t.Fatalf("generation %q accepted", bad)
		}
	}
	if _, ok := DaemonEndpointID("", generation); ok {
		t.Fatal("empty session accepted")
	}
}
