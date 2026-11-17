package main

import (
	"time"

	edgetelemetry "github.com/pinksaucepasta/paperboat/edge/internal/telemetry"
	"github.com/pinksaucepasta/paperboat/shared/nodemetrics"
)

// nodeMetrics reports this edge's version, CPU, memory and bytes carried with each heartbeat.
func nodeMetrics() func(time.Time) *nodemetrics.Report {
	sampler := nodemetrics.NewSampler()
	return func(at time.Time) *nodemetrics.Report {
		cpu, rss := sampler.Sample(at)
		in, out := edgetelemetry.TrafficTotals()
		return &nodemetrics.Report{Version: version, CPUPercent: cpu, RSSBytes: rss, BytesIn: in, BytesOut: out}
	}
}
