package selfhost

import "testing"

// A self-hosted component advertises its real node size with no ceiling; a missing size or an unknown capability is refused.
func TestComponentCapacityHasNoCeiling(t *testing.T) {
	component := func(capability string, capacity int64) Component {
		return Component{Capability: capability, TCPPort: 7272, QUICPort: 7273, Region: "eu-test", FailureDomain: "zone-a", CapacityLimit: capacity}
	}
	for _, test := range []struct {
		component Component
		want      bool
	}{
		{component("relay", 256), true},
		{component("relay", 100000), true},
		{component("relay", 0), false},
		{component("tunnel", 257), true},
		{component("tunnel", 100000), true},
		{component("console", 8), false},
	} {
		if got := validComponent(test.component); got != test.want {
			t.Errorf("%s capacity %d valid = %v, want %v", test.component.Capability, test.component.CapacityLimit, got, test.want)
		}
	}
}
