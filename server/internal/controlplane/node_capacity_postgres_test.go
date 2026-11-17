package controlplane

import (
	"context"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/shared/nodemetrics"
)

func capacityRow(t *testing.T, n *relaySubjectsNode) (in, out int64, cpu, sustained float64, rss int64, version string) {
	t.Helper()
	if err := n.store.SQL().QueryRowContext(t.Context(), `SELECT bytes_in_total,bytes_out_total,cpu_percent,cpu_sustained_percent,rss_bytes,version FROM node_capacity WHERE node_id=$1`, n.id).Scan(&in, &out, &cpu, &sustained, &rss, &version); err != nil {
		t.Fatal(err)
	}
	return
}

func dayTotals(t *testing.T, n *relaySubjectsNode, day string) (in, out int64, cpuMax float64, rssMax int64) {
	t.Helper()
	if err := n.store.SQL().QueryRowContext(t.Context(), `SELECT coalesce(sum(bytes_in),0),coalesce(sum(bytes_out),0),coalesce(max(cpu_max),0),coalesce(max(rss_max),0) FROM node_capacity_daily WHERE node_id=$1 AND day=$2::date`, n.id, day).Scan(&in, &out, &cpuMax, &rssMax); err != nil {
		t.Fatal(err)
	}
	return
}

// Bytes are cumulative per process epoch: the first sample is only a baseline, later ones add their difference to the
// day, a new epoch counts from zero, a sample inside the write interval is skipped without losing bytes, and CPU is averaged.
func TestRecordNodeCapacityKeepsDailyTotalsFromCumulativeCounters(t *testing.T) {
	n := newRelaySubjectsNode(t, "capacity")
	t.Cleanup(func() {
		n.store.SQL().ExecContext(context.Background(), `DELETE FROM node_capacity WHERE node_id=$1`, n.id)
		n.store.SQL().ExecContext(context.Background(), `DELETE FROM node_capacity_daily WHERE node_id=$1`, n.id)
	})
	conns := int64(7)
	at := time.Date(2026, 11, 10, 23, 59, 0, 0, time.UTC)
	record := func(epoch string, seconds int, in, out uint64, cpu float64) {
		t.Helper()
		err := RecordNodeCapacity(t.Context(), n.store, NodeSample{NodeID: n.id, Role: "edge", ProcessEpoch: epoch, Connections: &conns,
			Metrics: nodemetrics.Report{Version: "dev-1", CPUPercent: cpu, RSSBytes: 100 << 20, BytesIn: in, BytesOut: out}}, at.Add(time.Duration(seconds)*time.Second))
		if err != nil {
			t.Fatal(err)
		}
	}
	record("e1", 0, 1000, 2000, 10) // baseline: nothing counted for the first sight of a node
	if in, out, _, _ := dayTotals(t, n, "2026-11-10"); in != 0 || out != 0 {
		t.Fatalf("baseline counted %d/%d", in, out)
	}
	record("e1", 10, 9000, 9000, 90) // inside the write interval: skipped
	if in, _, _, _, _, _ := capacityRow(t, n); in != 1000 {
		t.Fatalf("a sample inside the interval was stored: %d", in)
	}
	record("e1", 30, 1500, 2600, 50) // same day: +500 in, +600 out
	if in, out, cpuMax, _ := dayTotals(t, n, "2026-11-10"); in != 500 || out != 600 || cpuMax != 50 {
		t.Fatalf("day one: in=%d out=%d cpuMax=%v", in, out, cpuMax)
	}
	record("e1", 90, 2500, 3600, 50) // after midnight UTC: the next day's row
	if in, out, _, _ := dayTotals(t, n, "2026-11-11"); in != 1000 || out != 1000 {
		t.Fatalf("day two: in=%d out=%d", in, out)
	}
	record("e2", 150, 40, 70, 20) // a restart: the new process counts from zero, its totals are all new bytes
	if in, out, _, _ := dayTotals(t, n, "2026-11-11"); in != 1040 || out != 1070 {
		t.Fatalf("after restart: in=%d out=%d", in, out)
	}
	record("e2", 210, 10, 10, 20) // a counter that went backwards inside one epoch counts as a reset too
	if in, out, _, _ := dayTotals(t, n, "2026-11-11"); in != 1050 || out != 1080 {
		t.Fatalf("after backwards counter: in=%d out=%d", in, out)
	}
	// One direction restarting does not recount the other: in goes 10 -> 510 (+500), out goes 10 -> 5 (reset, +5).
	record("e2", 270, 510, 5, 20)
	if in, out, _, _ := dayTotals(t, n, "2026-11-11"); in != 1550 || out != 1085 {
		t.Fatalf("mixed reset: in=%d out=%d", in, out)
	}
	// A node that rotates its epoch with a huge total is clamped to what a link could carry in the elapsed time.
	record("e3", 330, uint64(1)<<60, 0, 20)
	if in, _, _, _ := dayTotals(t, n, "2026-11-11"); in != 1550+60*maxBytesPerSecond {
		t.Fatalf("an implausible total was not clamped: in=%d", in)
	}
	record("e3", 390, 1<<60+100, 0, 20)
	in, out, cpu, sustained, rss, version := capacityRow(t, n)
	if in != 1<<60+100 || out != 0 || cpu != 20 || rss != 100<<20 || version != "dev-1" {
		t.Fatalf("latest: %d %d %v %d %q", in, out, cpu, rss, version)
	}
	// 5 minute time constant, samples a minute apart: the average moves toward the samples but lags a spike.
	if sustained <= 20 || sustained >= 50 {
		t.Fatalf("sustained cpu %v should lag between the samples 50 and 20", sustained)
	}
}

// The heartbeat paths take the report from the wire: a relay observation stores it, an out-of-range one is refused.
func TestRelayObservationStoresAndBoundsNodeMetrics(t *testing.T) {
	n := newRelaySubjectsNode(t, "capwire")
	t.Cleanup(func() {
		n.store.SQL().ExecContext(context.Background(), `DELETE FROM node_capacity WHERE node_id=$1`, n.id)
		n.store.SQL().ExecContext(context.Background(), `DELETE FROM node_capacity_daily WHERE node_id=$1`, n.id)
	})
	good := &nodemetrics.Report{Version: "dev-relay", CPUPercent: 12, RSSBytes: 300 << 20, BytesOut: 5000}
	if _, err := n.service.ObserveRelayNode(t.Context(), relayNodeObservation{NodeID: n.id, Generation: n.generation, ProcessEpoch: n.epoch, Ready: true, CapacityUsed: 4, Metrics: good}); err != nil {
		t.Fatal(err)
	}
	var role, version string
	var connections int64
	if err := n.store.SQL().QueryRowContext(t.Context(), `SELECT role,version,connections FROM node_capacity WHERE node_id=$1`, n.id).Scan(&role, &version, &connections); err != nil || role != "relay" || version != "dev-relay" || connections != 4 {
		t.Fatalf("stored %q %q %d %v", role, version, connections, err)
	}
	// An out-of-range report is ignored, not a reason to fail the heartbeat: the node is alive and keeps its lease.
	bad := &nodemetrics.Report{CPUPercent: 400, Version: "must-not-be-stored"}
	if _, err := n.service.ObserveRelayNode(t.Context(), relayNodeObservation{NodeID: n.id, Generation: n.generation, ProcessEpoch: n.epoch, Ready: true, Metrics: bad}); err != nil {
		t.Fatalf("a faulty report failed the observation: %v", err)
	}
	if err := n.store.SQL().QueryRowContext(t.Context(), `SELECT version FROM node_capacity WHERE node_id=$1`, n.id).Scan(&version); err != nil || version != "dev-relay" {
		t.Fatalf("a faulty report replaced the stored one: %q %v", version, err)
	}
	// An older relay sends no report: the observation still works.
	if _, err := n.service.ObserveRelayNode(t.Context(), relayNodeObservation{NodeID: n.id, Generation: n.generation, ProcessEpoch: n.epoch, Ready: true}); err != nil {
		t.Fatalf("an observation without metrics failed: %v", err)
	}
	if usableMetrics(t.Context(), n.id, bad) != nil || usableMetrics(t.Context(), n.id, good) == nil || usableMetrics(t.Context(), n.id, nil) != nil {
		t.Fatal("usableMetrics must drop only an out-of-range report")
	}
}

func TestCounterDeltaJudgesEachDirectionAlone(t *testing.T) {
	for _, c := range []struct {
		now, previous int64
		sameEpoch     bool
		want          int64
	}{
		{1500, 1000, true, 500}, {5, 1000, true, 5}, {40, 1000, false, 40}, {1000, 1000, true, 0}, {0, 0, false, 0},
	} {
		if got := counterDelta(c.now, c.previous, c.sameEpoch); got != c.want {
			t.Errorf("counterDelta(%d,%d,%v)=%d want %d", c.now, c.previous, c.sameEpoch, got, c.want)
		}
	}
	if maxNodeBytes(0) != maxBytesPerSecond || maxNodeBytes(time.Minute) != 60*maxBytesPerSecond {
		t.Fatal("maxNodeBytes must allow at least one second of traffic")
	}
}
