package controlplane

import (
	"testing"

	"github.com/pinksaucepasta/paperboat/shared/relaylimits"
)

// A relay registered at a real node size (thousands of connections) may start; the ceiling is the
// shared one the registry and the relay enforce, not a fixed 256.
func TestRelayNodeStartFollowsTheSharedCapacityCeiling(t *testing.T) {
	for _, test := range []struct {
		state    string
		capacity int64
		want     bool
	}{
		{"ready", 256, true},
		{"registered", 4096, true},
		{"ready", relaylimits.MaxConnectionsPerNode, true},
		{"ready", relaylimits.MaxConnectionsPerNode + 1, false},
		{"ready", 0, false},
		{"draining", 4096, false},
		{"retired", 4096, false},
	} {
		if got := relayNodeMayStart(test.state, test.capacity); got != test.want {
			t.Errorf("state %s capacity %d may start = %v, want %v", test.state, test.capacity, got, test.want)
		}
	}
}
