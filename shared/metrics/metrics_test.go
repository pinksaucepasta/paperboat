package metrics

import (
	"math"
	"testing"

	"github.com/pinksaucepasta/paperboat/shared/health"
	"github.com/pinksaucepasta/paperboat/shared/telemetry/safe"
)

var testDescriptors = []Descriptor{
	{Name: "t_requests_total", Kind: Counter, Labels: []LabelDescriptor{{Name: "outcome", AllowedValues: []string{"success", "failed"}}}},
	{Name: "t_depth", Kind: Gauge},
	{Name: "t_latency_seconds", Kind: Histogram, Labels: []LabelDescriptor{{Name: "op", AllowedValues: []string{"read"}}}, Histogram: &HistogramDescriptor{Buckets: []float64{0.1, 1}}},
}

func TestMetricsRecordSnapshotDeterministicAndRejectBadInput(t *testing.T) {
	m := New(testDescriptors, 0)
	if err := m.AddCounter("t_requests_total", Labels{"outcome": "success"}, 3); err != nil {
		t.Fatal(err)
	}
	if err := m.SetGauge("t_depth", nil, 7); err != nil {
		t.Fatal(err)
	}
	if err := m.ObserveHistogram("t_latency_seconds", Labels{"op": "read"}, 0.5); err != nil {
		t.Fatal(err)
	}
	first, _ := m.JSON()
	second, _ := m.JSON()
	if string(first) != string(second) || len(m.Snapshot()) != 3 {
		t.Fatalf("snapshot not deterministic: %s", first)
	}
	cases := []struct {
		name string
		err  error
		code safe.ErrorCode
	}{
		{"unknown metric", m.AddCounter("nope", nil, 1), safe.ErrorUnknownMetric},
		{"kind mismatch", m.SetGauge("t_requests_total", Labels{"outcome": "success"}, 1), safe.ErrorMetricKindMismatch},
		{"unknown label", m.AddCounter("t_requests_total", Labels{"outcome": "success", "host": "x"}, 1), safe.ErrorUnknownLabel},
		{"missing label", m.AddCounter("t_requests_total", nil, 1), safe.ErrorMissingLabel},
		{"value rejected", m.AddCounter("t_requests_total", Labels{"outcome": "customer.example.com"}, 1), safe.ErrorLabelValueRejected},
		{"NaN", m.ObserveHistogram("t_latency_seconds", Labels{"op": "read"}, math.NaN()), safe.ErrorInvalidObservation},
	}
	for _, c := range cases {
		if safe.ErrorCodeOf(c.err) != c.code {
			t.Errorf("%s: error = %v, want %s", c.name, c.err, c.code)
		}
	}
}

func TestMetricsBoundSeriesAndCloneDescriptors(t *testing.T) {
	m := New(testDescriptors, 1)
	if err := m.AddCounter("t_requests_total", Labels{"outcome": "success"}, 1); err != nil {
		t.Fatal(err)
	}
	if err := m.AddCounter("t_requests_total", Labels{"outcome": "failed"}, 1); safe.ErrorCodeOf(err) != safe.ErrorMetricCardinality {
		t.Fatalf("second series error = %v", err)
	}
	cloned := Clone(testDescriptors)
	cloned[0].Labels[0].AllowedValues[0] = "mutated"
	if testDescriptors[0].Labels[0].AllowedValues[0] != "success" {
		t.Fatal("Clone shares label values")
	}
}

func TestContractDescriptorsAreWellFormed(t *testing.T) {
	seen := map[string]bool{}
	for _, d := range ContractDescriptors() {
		if seen[d.Name] || d.Name == "" {
			t.Fatalf("duplicate or empty name %q", d.Name)
		}
		seen[d.Name] = true
		if (d.Kind == Histogram) != (d.Histogram != nil) {
			t.Fatalf("%s: histogram buckets do not match kind %s", d.Name, d.Kind)
		}
	}
	if len(seen) != 9 {
		t.Fatalf("contract has %d descriptors, want 9", len(seen))
	}
	if got := len(DimensionValues()); got != len(health.Dimensions()) {
		t.Fatalf("dimension values = %d, want %d", got, len(health.Dimensions()))
	}
}
