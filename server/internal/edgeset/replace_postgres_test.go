package edgeset

import (
	"slices"
	"testing"
	"time"
)

// wakes is the sum of the machine's tunnels and previews topic generations: each wake adds one per topic.
func (f *nearestFixture) wakes(machine string) int {
	f.t.Helper()
	var n int
	if err := f.db.Pool().QueryRow(f.ctx, `SELECT coalesce(sum(generation),0) FROM paperboat.machine_topic_generations WHERE machine_id=$1 AND topic IN ('tunnels','previews')`, machine).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

// Disabling a region drains its nodes and re-places its workspaces at once: every workspace on it moves to the
// remaining edges and each machine is woken once; a replay moves nothing and wakes nobody (production 2026-11-21:
// 203 workspaces stayed on a disabled region until edited by hand).
func TestReplaceRegionMovesWorkspacesAndWakesOnceOnPostgres(t *testing.T) {
	f, drained, spare := settledPair(t)
	machine := "edgenear_machine_m_" + f.stamp
	f.machine("m2", map[string]int{}, "1 hour")
	machine2 := "edgenear_machine_m2_" + f.stamp
	before, before2 := f.wakes(machine), f.wakes(machine2)
	f.drain(drained, "30 seconds") // what region disable does: the sweep alone would keep it until the deadline
	moved, kept, err := ReplaceRegion(f.ctx, f.db, region(drained), time.Now())
	if err != nil || moved < 1 || kept != 0 {
		t.Fatalf("ReplaceRegion moved=%d kept=%d err=%v", moved, kept, err)
	}
	var got []string
	if err = f.db.Pool().QueryRow(f.ctx, `SELECT regions FROM paperboat.workspace_edge_sets WHERE scope_kind='account' AND scope_id=$1`, f.account).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || slices.Contains(got, region(drained)) || !slices.Contains(got, region(spare)) {
		t.Fatalf("pair=%v, want %s replaced by %s", got, region(drained), region(spare))
	}
	if f.wakes(machine) != before+2 || f.wakes(machine2) != before2+2 {
		t.Fatalf("wakes %d->%d, %d->%d: want one notice (tunnels+previews) per machine", before, f.wakes(machine), before2, f.wakes(machine2))
	}
	moved, kept, err = ReplaceRegion(f.ctx, f.db, region(drained), time.Now())
	if err != nil || moved != 0 || kept != 0 || f.wakes(machine) != before+2 || f.wakes(machine2) != before2+2 {
		t.Fatalf("replay moved=%d kept=%d err=%v wakes=%d: want nothing moved and no second notice", moved, kept, err, f.wakes(machine))
	}
}

// The sweep's replacement wakes the machines too: before, it stored the new pair and told nobody.
func TestSweepReplacementWakesMachinesOnPostgres(t *testing.T) {
	f, drained, _ := settledPair(t)
	machine := "edgenear_machine_m_" + f.stamp
	before := f.wakes(machine)
	f.drain(drained, "-1 second")
	if _, err := Sweep(f.ctx, f.db, time.Now(), true); err != nil {
		t.Fatal(err)
	}
	if got := f.wakes(machine); got != before+2 {
		t.Fatalf("wakes %d->%d after the sweep moved the pair, want one notice", before, got)
	}
}
