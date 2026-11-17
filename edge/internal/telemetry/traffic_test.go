package telemetry

import "testing"

func TestTrafficTotalsCountEachDirection(t *testing.T) {
	in0, out0 := TrafficTotals()
	RecordTraffic("ingress", 10)
	RecordTraffic("egress", 4)
	RecordTraffic("egress", 1)
	RecordTraffic("sideways", 99)
	in, out := TrafficTotals()
	if in-in0 != 10 || out-out0 != 5 {
		t.Fatalf("ingress +%d egress +%d", in-in0, out-out0)
	}
}
