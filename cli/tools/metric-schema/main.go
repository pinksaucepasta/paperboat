package main

import (
	"fmt"
	"net/http/httptest"
	"sort"

	hosttelemetry "github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/telemetry"
	"github.com/pinksaucepasta/paperboat/shared/metrics/schema"
)

func main() { schema.Main(buildMetrics, verifyHandler) }

func buildMetrics() ([]schema.Metric, error) {
	descriptors := hosttelemetry.DefaultDescriptors()
	metrics := make([]schema.Metric, 0, len(descriptors))
	for _, descriptor := range descriptors {
		metric := schema.Metric{Name: descriptor.Name, Kind: string(descriptor.Kind)}
		if len(descriptor.Labels) != 0 {
			metric.Labels = make(map[string][]string, len(descriptor.Labels))
			for label, values := range descriptor.Labels {
				for value := range values {
					metric.Labels[label] = append(metric.Labels[label], value)
				}
			}
		}
		if descriptor.Kind == hosttelemetry.Histogram {
			metric.Buckets = hosttelemetry.HistogramBuckets()
		}
		metrics = append(metrics, metric)
	}
	return metrics, nil
}

// verifyHandler records one observation of every descriptor and checks the
// registry handler's text against the document.
func verifyHandler(document schema.Document) error {
	descriptors := hosttelemetry.DefaultDescriptors()
	registry, err := hosttelemetry.NewRegistry(descriptors)
	if err != nil {
		return err
	}
	for _, descriptor := range descriptors {
		labels := make(map[string]string, len(descriptor.Labels))
		for label, values := range descriptor.Labels {
			allowed := make([]string, 0, len(values))
			for value := range values {
				allowed = append(allowed, value)
			}
			sort.Strings(allowed)
			labels[label] = allowed[0]
		}
		if err := registry.Record(descriptor.Name, 1, labels); err != nil {
			return fmt.Errorf("record %s: %w", descriptor.Name, err)
		}
	}
	recorder := httptest.NewRecorder()
	registry.Handler().ServeHTTP(recorder, httptest.NewRequest("GET", "/metrics", nil))
	if recorder.Code != 200 {
		return fmt.Errorf("status %d", recorder.Code)
	}
	return schema.VerifyPrometheus(document, recorder.Body.String())
}
