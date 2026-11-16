package main

import (
	"testing"
	"time"
)

func TestStallCheckFlagsTenSecondsOfSilence(t *testing.T) {
	start := time.Unix(1000, 0)
	if _, stalled := stallCheck(start, start.Add(9*time.Second), start.Add(10*time.Second)); stalled {
		t.Fatal("10 s of silence must not stall yet")
	}
	if _, stalled := stallCheck(start, start.Add(10*time.Second), start.Add(11*time.Second)); !stalled {
		t.Fatal("11 s of silence must stall")
	}
}

func TestStallCheckRestartsAfterFrozenPage(t *testing.T) {
	start := time.Unix(1000, 0)
	// The worker slept 60 s (a frozen tab): the machine was not given a chance to answer.
	heard, stalled := stallCheck(start, start.Add(time.Second), start.Add(61*time.Second))
	if stalled || !heard.Equal(start.Add(61*time.Second)) {
		t.Fatalf("frozen page: stalled=%v heard=%v, want count restarted", stalled, heard)
	}
}
