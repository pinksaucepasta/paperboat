package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/shared/nodemetrics"
)

const (
	// CPUSustainedWindow is the time constant of the CPU average "sustained" warnings read.
	CPUSustainedWindow = 5 * time.Minute
	// nodeCapacityEvery is how often a node's sample is written: bytes are cumulative, so skipping heartbeats loses nothing.
	nodeCapacityEvery = 25 * time.Second
)

// NodeSample is one report of a node: who, in which process epoch, how many connections it holds (nil when the node
// has no such notion) and its nodemetrics.Report.
type NodeSample struct {
	NodeID, Role, ProcessEpoch string
	Connections                *int64
	Metrics                    nodemetrics.Report
}

// RecordNodeCapacity stores a node's latest sample and adds the bytes it carried since its previous sample to
// today's total (UTC). It writes at most every nodeCapacityEvery per node. The first sample of a node only sets the
// baseline; a new process epoch counts its totals from zero, because a restarted process starts counting at zero.
func RecordNodeCapacity(ctx context.Context, store *db.DB, sample NodeSample, now time.Time) error {
	now = now.UTC()
	return store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		var (
			epoch           string
			sustained       float64
			prevIn, prevOut int64
			reportedAt      time.Time
			existing        = true
		)
		err := tx.QueryRow(ctx, `SELECT process_epoch,cpu_sustained_percent,bytes_in_total,bytes_out_total,reported_at FROM paperboat.node_capacity WHERE node_id=$1 FOR UPDATE`, sample.NodeID).
			Scan(&epoch, &sustained, &prevIn, &prevOut, &reportedAt)
		if err != nil {
			if !noRows(err) {
				return err
			}
			existing = false
		}
		if existing && epoch == sample.ProcessEpoch && now.Sub(reportedAt) < nodeCapacityEvery {
			return nil
		}
		var deltaIn, deltaOut int64
		if existing {
			sameEpoch := epoch == sample.ProcessEpoch
			ceiling := maxNodeBytes(now.Sub(reportedAt))
			deltaIn = min(counterDelta(int64(sample.Metrics.BytesIn), prevIn, sameEpoch), ceiling)
			deltaOut = min(counterDelta(int64(sample.Metrics.BytesOut), prevOut, sameEpoch), ceiling)
		}
		cpu := float64(sample.Metrics.CPUPercent)
		if !existing {
			sustained = cpu
		} else if dt := now.Sub(reportedAt).Seconds(); dt > 0 {
			sustained += (1 - math.Exp(-dt/CPUSustainedWindow.Seconds())) * (cpu - sustained)
		}
		if _, err = tx.Exec(ctx, `INSERT INTO paperboat.node_capacity(node_id,role,process_epoch,version,cpu_percent,cpu_sustained_percent,rss_bytes,connections,bytes_in_total,bytes_out_total,reported_at)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
 ON CONFLICT (node_id) DO UPDATE SET role=EXCLUDED.role,process_epoch=EXCLUDED.process_epoch,version=EXCLUDED.version,cpu_percent=EXCLUDED.cpu_percent,
  cpu_sustained_percent=EXCLUDED.cpu_sustained_percent,rss_bytes=EXCLUDED.rss_bytes,connections=EXCLUDED.connections,
  bytes_in_total=EXCLUDED.bytes_in_total,bytes_out_total=EXCLUDED.bytes_out_total,reported_at=EXCLUDED.reported_at`,
			sample.NodeID, sample.Role, sample.ProcessEpoch, sample.Metrics.Version, cpu, sustained, int64(sample.Metrics.RSSBytes), sample.Connections,
			int64(sample.Metrics.BytesIn), int64(sample.Metrics.BytesOut), now); err != nil {
			return err
		}
		connections := int64(0)
		if sample.Connections != nil {
			connections = *sample.Connections
		}
		_, err = tx.Exec(ctx, `INSERT INTO paperboat.node_capacity_daily(node_id,day,bytes_in,bytes_out,cpu_max,rss_max,connections_max)
 VALUES($1,$2::date,$3,$4,$5,$6,$7)
 ON CONFLICT (node_id,day) DO UPDATE SET bytes_in=node_capacity_daily.bytes_in+EXCLUDED.bytes_in,bytes_out=node_capacity_daily.bytes_out+EXCLUDED.bytes_out,
  cpu_max=GREATEST(node_capacity_daily.cpu_max,EXCLUDED.cpu_max),rss_max=GREATEST(node_capacity_daily.rss_max,EXCLUDED.rss_max),
  connections_max=GREATEST(node_capacity_daily.connections_max,EXCLUDED.connections_max)`,
			sample.NodeID, now.Format("2006-01-02"), deltaIn, deltaOut, cpu, int64(sample.Metrics.RSSBytes), connections)
		if err != nil || sample.Role == "server" {
			return err
		}
		// The same bytes by hour and kind of node, for the operator traffic dashboard (a zero delta still marks the node as reporting).
		_, err = tx.Exec(ctx, `INSERT INTO paperboat.traffic_hourly(hour,category,node_id,upload_bytes,download_bytes)
 SELECT date_trunc('hour',$1::timestamptz),$2||CASE WHEN EXISTS(SELECT 1 FROM paperboat.selfhost_installations i WHERE i.node_id=$3 AND i.scope_kind IN ('account','team')) THEN '_self_hosted' ELSE '_managed' END,$3,$4,$5
 ON CONFLICT(hour,category,node_id,shard) DO UPDATE SET upload_bytes=traffic_hourly.upload_bytes+EXCLUDED.upload_bytes,download_bytes=traffic_hourly.download_bytes+EXCLUDED.download_bytes`,
			now, sample.Role, sample.NodeID, deltaIn, deltaOut)
		return err
	})
}

// maxBytesPerSecond bounds what one node can plausibly carry (100 Gbit/s). A report that claims more since the previous
// one (a node that rotates its process epoch, or a corrupt counter) is clamped, so one node cannot inflate its totals.
const maxBytesPerSecond = 12_500_000_000

func maxNodeBytes(since time.Duration) int64 {
	return int64(max(since, time.Second).Seconds()) * maxBytesPerSecond
}

// counterDelta is the bytes a cumulative counter gained since its stored value. Each direction is judged alone: one that
// went backwards (or belongs to a new process epoch) restarted at zero, so its whole value is new; the other is unaffected.
func counterDelta(now, previous int64, sameEpoch bool) int64 {
	if sameEpoch && now >= previous {
		return now - previous
	}
	return now
}

// recordNodeMetrics is the heartbeat handlers' call: a node's report is optional (an older node sends none) and a
// failure to store it never fails the heartbeat.
func (s *EdgeService) recordNodeMetrics(ctx context.Context, role, nodeID, epoch string, connections int64, report *nodemetrics.Report) {
	if report == nil {
		return
	}
	connections = min(connections, math.MaxInt32) // the column is an integer
	if err := RecordNodeCapacity(ctx, s.store, NodeSample{NodeID: nodeID, Role: role, ProcessEpoch: epoch, Connections: &connections, Metrics: *report}, s.clock()); err != nil {
		slog.WarnContext(ctx, "node capacity not recorded", "error", err.Error(), "node_id", nodeID, "role", role)
	}
}

func noRows(err error) bool { return errors.Is(err, pgx.ErrNoRows) || errors.Is(err, sql.ErrNoRows) }

// usableMetrics drops an out-of-range report instead of rejecting the heartbeat: a node with a faulty sampler is still
// alive, and must not look dead because of its statistics.
func usableMetrics(ctx context.Context, nodeID string, report *nodemetrics.Report) *nodemetrics.Report {
	if report == nil {
		return nil
	}
	if err := report.Validate(); err != nil {
		slog.WarnContext(ctx, "node metrics report ignored", "error", err.Error(), "node_id", nodeID)
		return nil
	}
	return report
}
