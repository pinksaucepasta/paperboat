package main

import (
	"testing"

	"github.com/pinksaucepasta/paperboat/cli/internal/api"
)

func TestRelayListShowsThisMachinesLatencyPerRegion(t *testing.T) {
	results := []relayListResult{
		{Region: "frankfurt", Source: "paperboat"}, {Region: "mumbai", Source: "paperboat"},
		{RelayID: "own", Region: "frankfurt", Source: "self-hosted"}, {RelayID: "other", Region: "home", Source: "self-hosted"},
	}
	setRelayLatency(results, []api.MachineLatency{
		{Region: "frankfurt", Role: "relay", RTTMillis: 42},
		{Region: "frankfurt", Role: "relay", SelfHosted: true, RTTMillis: 9},
		{Region: "mumbai", Role: "edge", RTTMillis: 7}, // an edge never fills a relay row
	})
	if results[0].LatencyMillis == nil || *results[0].LatencyMillis != 42 {
		t.Fatalf("Paperboat region = %+v", results[0])
	}
	if results[2].LatencyMillis == nil || *results[2].LatencyMillis != 9 {
		t.Fatalf("own relay of the region = %+v", results[2])
	}
	if results[1].LatencyMillis != nil || results[3].LatencyMillis != nil {
		t.Fatalf("unmeasured rows must stay blank: %+v %+v", results[1], results[3])
	}
}
