package supportreport

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/getsentry/sentry-go"
	serverdiagnostics "github.com/pinksaucepasta/paperboat-server/internal/diagnostics"
	"github.com/pinksaucepasta/paperboat-server/internal/telemetry"
)

func TestCombinedSnapshotSDKBatchBoundAndFullRotation(t *testing.T) {
	registry := telemetry.NewMetrics()
	for _, descriptor := range telemetry.MetricDescriptors() {
		var populate func(int, telemetry.MetricLabels)
		populate = func(index int, labels telemetry.MetricLabels) {
			if index < len(descriptor.Labels) {
				label := descriptor.Labels[index]
				for _, value := range label.AllowedValues {
					labels[label.Name] = value
					populate(index+1, labels)
				}
				delete(labels, label.Name)
				return
			}
			var err error
			switch descriptor.Kind {
			case telemetry.MetricCounter:
				err = registry.AddCounter(descriptor.Name, labels, 17)
			case telemetry.MetricHistogram:
				err = registry.ObserveHistogram(descriptor.Name, labels, .5)
			default:
				err = registry.SetGauge(descriptor.Name, labels, 23)
			}
			if err != nil {
				t.Fatal(err)
			}
		}
		populate(0, telemetry.MetricLabels{})
	}
	samples := registry.Snapshot()
	platform := serverdiagnostics.MetricsSnapshot()
	names := serverdiagnostics.MetricNames()
	transport := &sentry.MockTransport{}
	options := sentry.ClientOptions{Dsn: "https://public@example.invalid/1", Transport: transport, Release: "test", Environment: "test"}
	s := &Service{release: "test", environment: "test"}
	if err := s.configureSignals(Config{PlatformMetricNames: names}, &options); err != nil {
		t.Fatal(err)
	}
	var err error
	s.client, err = sentry.NewClient(options)
	if err != nil {
		t.Fatal(err)
	}
	defer s.client.Close()
	appNames := []string{"paperboat.database.connections", "paperboat.database.acquired", "paperboat.database.idle", "paperboat.database.acquire_wait_seconds_snapshot", "paperboat.database.acquire_canceled_snapshot", "paperboat.runtime.goroutines", "paperboat.runtime.heap_bytes"}
	s.SetSnapshot(func() {
		s.RegistrySnapshot(samples)
		s.PlatformSnapshot(platform)
		for _, name := range appNames {
			s.Gauge(name, 1)
		}
	})
	expected := map[string]float64{}
	for _, sample := range samples {
		labels := map[string]string{}
		for _, label := range sample.Labels {
			labels[label.Name] = label.Value
		}
		switch sample.Kind {
		case telemetry.MetricCounter:
			expected[snapshotIdentity(sample.Name+"_snapshot", labels)] = float64(sample.Value)
		case telemetry.MetricHistogram:
			expected[snapshotIdentity(sample.Name+"_sum_snapshot", labels)] = sample.Sum
			expected[snapshotIdentity(sample.Name+"_count_snapshot", labels)] = float64(sample.Count)
		default:
			expected[snapshotIdentity(sample.Name, labels)] = float64(sample.Value)
		}
	}
	for name, value := range platform {
		expected[snapshotIdentity("paperboat_server_"+name+"_snapshot", nil)] = float64(value)
	}
	for _, name := range appNames {
		expected[snapshotIdentity(name, nil)] = 1
	}
	expected[snapshotIdentity("paperboat.reporting.dropped", nil)] = 0
	expected[snapshotIdentity("paperboat.reporting.cleanup_failures", nil)] = 0
	if len(expected) <= snapshotBatchLimit {
		t.Fatal("fixture does not exercise combined burst overflow")
	}
	cycles := (len(expected) + snapshotBatchLimit - 1) / snapshotBatchLimit
	seen := map[string]bool{}
	previous := 0
	for cycle := 0; cycle < cycles; cycle++ {
		s.snapshotMetrics()
		if !s.client.FlushWithContext(context.Background()) {
			t.Fatal("SDK flush failed")
		}
		total := 0
		for _, event := range transport.Events() {
			for _, metric := range event.Metrics {
				total++
				labels := map[string]string{}
				for name, value := range metric.Attributes {
					switch name {
					case "component":
						if value.AsString() != "paperboat-server" {
							t.Fatal("snapshot component identity changed")
						}
					case "sentry.release", "sentry.environment":
						if value.AsString() != "test" {
							t.Fatal("snapshot SDK identity changed")
						}
					default:
						labels[name] = value.AsString()
					}
				}
				if metric.Attributes["sentry.release"].AsString() != "test" || metric.Attributes["sentry.environment"].AsString() != "test" {
					t.Fatal("snapshot SDK identity missing")
				}
				key := snapshotIdentity(metric.Name, labels)
				want, ok := expected[key]
				value, isFloat := metric.Value.Float64()
				if !ok || !isFloat || value != want {
					t.Fatalf("snapshot value or dimension changed: %s", key)
				}
				seen[key] = true
			}
		}
		if total-previous != snapshotBatchLimit {
			t.Fatalf("SDK batch count=%d want %d", total-previous, snapshotBatchLimit)
		}
		previous = total
	}
	if len(seen) != len(expected) {
		t.Fatalf("rotation exported %d/%d series", len(seen), len(expected))
	}
	wantDeferred := uint64(cycles * (len(expected) - snapshotBatchLimit))
	if got := s.SignalStatus()["snapshot_deferred"]; got != wantDeferred {
		t.Fatalf("deferred=%v want %d", got, wantDeferred)
	}
	if snapshotInterval != 5*time.Second {
		t.Fatal("snapshot cadence differs from qualified interval")
	}
	t.Logf("qualified %d combined series in %d SDK batches", len(expected), cycles)
}
func snapshotIdentity(name string, labels map[string]string) string {
	if len(labels) == 0 {
		return name
	}
	raw, _ := json.Marshal(labels)
	return name + string(raw)
}
