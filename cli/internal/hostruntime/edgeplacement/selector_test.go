package edgeplacement

import (
	"context"
	"errors"
	"testing"
	"time"
)

func candidate(id, region, domain string) Candidate {
	return Candidate{NodeID: id, ProcessEpoch: "epoch_" + id, Region: region, FailureDomain: domain, ProbeAddress: "127.0.0.1:38080"}
}

func TestPlacementKeepsPrimaryUntilMaterialSustainedGain(t *testing.T) {
	var selector Selector
	now := time.Now()
	a, b, c := candidate("a", "mumbai", "a-zone"), candidate("b", "mumbai", "b-zone"), candidate("c", "singapore", "c-zone")
	choose := func(offset time.Duration, aRTT, bRTT, cRTT time.Duration) Preference {
		t.Helper()
		value, err := selector.Select(now.Add(offset), []Measurement{{Candidate: a, RTT: aRTT}, {Candidate: b, RTT: bRTT}, {Candidate: c, RTT: cRTT}})
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	first := choose(0, 30*time.Millisecond, 35*time.Millisecond, 60*time.Millisecond)
	if first.PrimaryNodeID != "a" || first.SecondaryNodeID != "b" {
		t.Fatalf("initial placement=%+v", first)
	}
	if got := choose(30*time.Second, 30*time.Millisecond, 27*time.Millisecond, 60*time.Millisecond); got.PrimaryNodeID != "a" {
		t.Fatalf("jitter moved primary: %+v", got)
	}
	for i := 1; i <= 2; i++ {
		if got := choose(time.Duration(i+1)*30*time.Second, 35*time.Millisecond, 15*time.Millisecond, 60*time.Millisecond); got.PrimaryNodeID != "a" {
			t.Fatalf("premature move: %+v", got)
		}
	}
	if got := choose(120*time.Second, 35*time.Millisecond, 15*time.Millisecond, 60*time.Millisecond); got.PrimaryNodeID != "b" || got.SecondaryNodeID != "a" {
		t.Fatalf("sustained faster edge not selected: %+v", got)
	}
}

func TestPlacementImmediatelyReplacesLostPrimaryAndEpoch(t *testing.T) {
	var selector Selector
	now := time.Now()
	a, b := candidate("a", "mumbai", "a-zone"), candidate("b", "singapore", "b-zone")
	if _, err := selector.Select(now, []Measurement{{Candidate: a, RTT: 10 * time.Millisecond}, {Candidate: b, RTT: 30 * time.Millisecond}}); err != nil {
		t.Fatal(err)
	}
	if got, err := selector.Select(now.Add(time.Second), []Measurement{{Candidate: b, RTT: 30 * time.Millisecond}}); err != nil || got.PrimaryNodeID != "b" || got.SecondaryNodeID != "" {
		t.Fatalf("lost edge placement=%+v error=%v", got, err)
	}
	a.ProcessEpoch = "new_epoch"
	if got, err := selector.Select(now.Add(2*time.Second), []Measurement{{Candidate: a, RTT: 10 * time.Millisecond}, {Candidate: b, RTT: 30 * time.Millisecond}}); err != nil || got.PrimaryNodeID != "b" {
		t.Fatalf("recovered former edge stole ownership: %+v error=%v", got, err)
	}
}

func TestPlacementDoesNotInventIndependentBackupForUnknownDomain(t *testing.T) {
	var selector Selector
	a := candidate("a", "mumbai", "")
	b := candidate("b", "singapore", "b-zone")
	placement, err := selector.Select(time.Now(), []Measurement{{Candidate: a, RTT: 10 * time.Millisecond}, {Candidate: b, RTT: 30 * time.Millisecond}})
	if err != nil || placement.PrimaryNodeID != "a" || placement.SecondaryNodeID != "" {
		t.Fatalf("unknown failure domain yielded backup: %+v error=%v", placement, err)
	}
}

func TestProbeBoundedAndCanceled(t *testing.T) {
	candidates := []Candidate{candidate("a", "a", "a"), candidate("b", "b", "b")}
	measurements, err := Probe(t.Context(), candidates, func(_ context.Context, address string) error {
		if address != "127.0.0.1:38080" {
			t.Fatalf("unexpected address %q", address)
		}
		return nil
	})
	if err != nil || len(measurements) != 2 {
		t.Fatalf("measurements=%d error=%v", len(measurements), err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := Probe(ctx, candidates, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled probe error=%v", err)
	}
}
