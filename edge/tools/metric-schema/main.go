package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"time"

	edgediagnostics "github.com/pinksaucepasta/paperboat/edge/internal/diagnostics"
	"github.com/pinksaucepasta/paperboat/edge/internal/node"
	edgetelemetry "github.com/pinksaucepasta/paperboat/edge/internal/telemetry"
	"github.com/pinksaucepasta/paperboat/edge/internal/usage"
	sharedmetrics "github.com/pinksaucepasta/paperboat/shared/metrics"
	"github.com/pinksaucepasta/paperboat/shared/metrics/schema"
)

func main() { schema.Main(buildMetrics, verifyHandler) }

func buildMetrics() ([]schema.Metric, error) { return edgediagnostics.MetricDescriptors(), nil }

// verifyHandler records one observation of every typed metric, then checks the
// diagnostics handler's text against the document.
func verifyHandler(document schema.Document) error {
	now := time.Unix(1_800_000_000, 0).UTC()
	typedMetrics := edgetelemetry.NewMetrics()
	for _, descriptor := range edgetelemetry.MetricDescriptors() {
		labels := make(sharedmetrics.Labels, len(descriptor.Labels))
		for _, label := range descriptor.Labels {
			if len(label.AllowedValues) == 0 {
				return fmt.Errorf("metric %q label %q has no allowed values", descriptor.Name, label.Name)
			}
			labels[label.Name] = label.AllowedValues[0]
		}
		var err error
		switch descriptor.Kind {
		case sharedmetrics.Counter:
			err = typedMetrics.AddCounter(descriptor.Name, labels, 1)
		case sharedmetrics.Gauge:
			err = typedMetrics.SetGauge(descriptor.Name, labels, 1)
		case sharedmetrics.Histogram:
			err = typedMetrics.ObserveHistogram(descriptor.Name, labels, descriptor.Histogram.Buckets[0])
		default:
			return fmt.Errorf("metric %q has unsupported kind %q", descriptor.Name, descriptor.Kind)
		}
		if err != nil {
			return fmt.Errorf("populate metric %q: %w", descriptor.Name, err)
		}
	}
	handler, err := edgediagnostics.NewHandler(edgediagnostics.Sources{
		Node:     func() node.Snapshot { return node.Snapshot{Live: true, Ready: true} },
		Manager:  func() node.ManagerSnapshot { return node.ManagerSnapshot{Capacity: 8} },
		Sessions: func() int { return 1 }, SessionRoutes: func() int { return 1 },
		ActiveStreams: func() uint32 { return 1 }, RouteCount: func() int { return 1 },
		Usage:      func() usage.QueueStats { return usage.QueueStats{MaxReports: 8, MaxBytes: 1024} },
		ControlErr: func() error { return nil }, RouteErr: func() error { return nil }, UsageErr: func() error { return nil },
		CarrierRunning: func() bool { return true },
		Traffic:        func() []usage.CounterRecord { return nil }, TypedMetrics: typedMetrics.Snapshot, Now: func() time.Time { return now },
	})
	if err != nil {
		return err
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if recorder.Code != http.StatusOK {
		return fmt.Errorf("status %d", recorder.Code)
	}
	return schema.VerifyPrometheus(document, recorder.Body.String())
}
