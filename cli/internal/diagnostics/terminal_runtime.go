package diagnostics

import (
	"math"
	"runtime/metrics"
	"time"

	"github.com/pinksaucepasta/paperboat/shared/diagnosticlog"
)

// StartTerminalRuntimeCapture samples pause deltas only on explicitly instrumented processes.
// Histogram upper bounds retain scheduler/GC evidence without stacks or application values.
func StartTerminalRuntimeCapture(role string) func() {
	if !TerminalHopsEnabled() {
		return func() {}
	}
	if !safeIdentifier(role, 64) {
		role = "unknown"
	}
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		samples := []metrics.Sample{{Name: "/sched/pauses/total/gc:seconds"}, {Name: "/sched/latencies:seconds"}, {Name: "/gc/cycles/total:gc-cycles"}}
		prior := make([][]uint64, 2)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			metrics.Read(samples)
			bounds := [2]uint64{}
			for i := range prior {
				if samples[i].Value.Kind() != metrics.KindFloat64Histogram {
					continue
				}
				h := samples[i].Value.Float64Histogram()
				if len(prior[i]) == len(h.Counts) {
					bounds[i] = histogramDeltaUpperUS(h, prior[i])
				}
				prior[i] = append(prior[i][:0], h.Counts...)
			}
			cycles := uint64(0)
			if samples[2].Value.Kind() == metrics.KindUint64 {
				cycles = samples[2].Value.Uint64()
			}
			diagnosticlog.TryInfo("terminal runtime sample", "reason", role, "gc_pause_us", bounds[0], "sched_pause_us", bounds[1], "gc_count", cycles, "dropped", diagnosticlog.Dropped())
			select {
			case <-stop:
				return
			case <-ticker.C:
			}
		}
	}()
	return func() { close(stop); <-done }
}

func histogramDeltaUpperUS(h *metrics.Float64Histogram, prior []uint64) uint64 {
	var upper float64
	for i, count := range h.Counts {
		if count > prior[i] {
			upper = h.Buckets[i+1]
			if math.IsInf(upper, 1) {
				return math.MaxUint64
			}
		}
	}
	return uint64(math.Ceil(upper * 1e6))
}
