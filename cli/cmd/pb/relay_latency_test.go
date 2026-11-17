package main

import (
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
)

func TestRelayListShowsThisMachinesLatency(t *testing.T) {
	measured := time.Unix(1_800_000_000, 0)
	results := []relayListResult{{RelayID: "relay_a"}, {RelayID: "relay_b"}, {RelayID: "relay_c"}}
	setRelayLatency(results, []api.MachineLatency{
		{NodeID: "relay_a", Role: "relay", RTTMillis: 42, MeasuredAt: measured},
		{NodeID: "relay_c", Role: "edge", RTTMillis: 7, MeasuredAt: measured}, // an edge never fills a relay row
	})
	if results[0].LatencyMillis == nil || *results[0].LatencyMillis != 42 || results[0].LatencyMeasuredAt != measured.Unix() {
		t.Fatalf("measured relay = %+v", results[0])
	}
	if results[1].LatencyMillis != nil || results[2].LatencyMillis != nil {
		t.Fatalf("unmeasured relays must stay blank: %+v %+v", results[1], results[2])
	}
}
