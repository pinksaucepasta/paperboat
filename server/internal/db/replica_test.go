package db

import (
	"testing"
	"time"
)

// A replica leaves rotation at once on lag above the bound or a failed sample, and rejoins only after 30 s
// of samples under the bound.
func TestReplicaRotationLeavesAtOnceAndRejoinsAfterThirtySeconds(t *testing.T) {
	r := &replicaState{}
	t0 := time.Unix(1_000_000, 0)
	r.observe(0, true, t0)
	if r.available() {
		t.Fatal("a fresh replica served reads before 30 s under the bound")
	}
	r.observe(time.Second, true, t0.Add(29*time.Second))
	if r.available() {
		t.Fatal("rejoined before 30 s")
	}
	r.observe(time.Second, true, t0.Add(30*time.Second))
	if !r.available() {
		t.Fatal("did not join after 30 s under the bound")
	}
	r.observe(3*time.Second, true, t0.Add(31*time.Second))
	if r.available() {
		t.Fatal("stayed in rotation with 3 s lag")
	}
	r.observe(0, true, t0.Add(32*time.Second))
	r.observe(0, false, t0.Add(40*time.Second))
	r.observe(0, true, t0.Add(41*time.Second))
	if r.observe(0, true, t0.Add(70*time.Second)); r.available() {
		t.Fatal("a failed sample did not restart the 30 s rejoin window")
	}
	if r.observe(0, true, t0.Add(71*time.Second)); !r.available() {
		t.Fatal("did not rejoin 30 s after the failed sample")
	}
}
