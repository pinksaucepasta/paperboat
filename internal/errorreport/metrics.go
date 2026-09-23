package errorreport

import (
	"context"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/getsentry/sentry-go"
	"github.com/getsentry/sentry-go/attribute"
)

// MetricSample contains a projection already checked against the owning registry's
// built-in descriptors. Custom descriptors and private IDs must never reach it.
type MetricSample struct {
	Name   string
	Value  float64
	Labels map[string]string
}

type MetricDescriptor struct {
	Name   string
	Labels map[string]map[string]bool
}

func (r *Reporter) RegisterMetrics(source func() []MetricSample, descriptors []MetricDescriptor) {
	if !r.Enabled() || !r.metrics || source == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.sources) < 4 && len(descriptors) <= 256 {
		for _, d := range descriptors {
			labels := map[string]map[string]bool{}
			for k, values := range d.Labels {
				copyValues := map[string]bool{}
				for v, ok := range values {
					copyValues[v] = ok
				}
				labels[k] = copyValues
			}
			r.metricSchema[d.Name] = labels
		}
		r.sources = append(r.sources, source)
	}
}
func (r *Reporter) sampleMetrics() {
	r.mu.Lock()
	sources := append([]func() []MetricSample(nil), r.sources...)
	r.mu.Unlock()
	all := []MetricSample{}
	for _, source := range sources {
		samples := source()
		if len(samples) > 8192 {
			r.snapshotDropped.Add(uint64(len(samples) - 8192))
			samples = samples[:8192]
		}
		all = append(all, samples...)
	}
	sort.Slice(all, func(i, j int) bool { return metricSampleKey(all[i]) < metricSampleKey(all[j]) })
	limit := len(all)
	if limit > snapshotLimit {
		r.snapshotDropped.Add(uint64(limit - snapshotLimit))
		limit = snapshotLimit
	}
	if limit == 0 {
		return
	}
	r.mu.Lock()
	start := r.snapshotCursor % len(all)
	r.snapshotCursor = (start + limit) % len(all)
	r.mu.Unlock()
	for index := 0; index < limit; index++ {
		sample := all[(start+index)%len(all)]
		if math.IsNaN(sample.Value) || math.IsInf(sample.Value, 0) {
			r.snapshotDropped.Add(1)
			continue
		}
		meter := sentry.NewMeter(r.context(context.Background()))
		attrs := []attribute.Builder{attribute.String("component", "paperboat-daemon")}
		for k, v := range sample.Labels {
			attrs = append(attrs, attribute.String("dimension."+k, v))
		}
		meter.SetAttributes(attrs...)
		meter.Gauge(sample.Name, sample.Value)
	}

}
func (r *Reporter) runMetrics() {
	defer close(r.samplerDone)
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			r.sampleMetrics()
		case <-r.samplerStop:
			r.sampleMetrics()
			return
		}
	}
}

// One snapshot batch fits the SDK metric processor's 100-item queue.
const snapshotLimit = 100

func (r *Reporter) SnapshotDropped() uint64 {
	if r == nil {
		return 0
	}
	return r.snapshotDropped.Load()
}
func metricSampleKey(s MetricSample) string {
	keys := make([]string, 0, len(s.Labels))
	for k := range s.Labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString(s.Name)
	for _, k := range keys {
		b.WriteByte('|')
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(s.Labels[k])
	}
	return b.String()
}
