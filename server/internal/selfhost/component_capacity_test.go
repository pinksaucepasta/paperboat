package selfhost

import (
	"testing"

	"github.com/pinksaucepasta/paperboat/shared/relaylimits"
)

// A self-hosted relay may advertise a real node size, up to the ceiling the relay enforces; a tunnel
// keeps its own bound.
func TestComponentCapacityFollowsTheCapabilityCeiling(t *testing.T) {
	component := func(capability string, capacity int64) Component {
		return Component{Capability: capability, TCPPort: 27443, QUICPort: 27444, Region: "eu-test", FailureDomain: "zone-a", CapacityLimit: capacity}
	}
	for _, test := range []struct {
		component Component
		want      bool
	}{
		{component("relay", 256), true},
		{component("relay", 4096), true},
		{component("relay", relaylimits.MaxConnectionsPerNode), true},
		{component("relay", relaylimits.MaxConnectionsPerNode+1), false},
		{component("relay", 0), false},
		{component("tunnel", 256), true},
		{component("tunnel", 257), false},
		{component("console", 8), false},
	} {
		if got := validComponent(test.component); got != test.want {
			t.Errorf("%s capacity %d valid = %v, want %v", test.component.Capability, test.component.CapacityLimit, got, test.want)
		}
	}
}
