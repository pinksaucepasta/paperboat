package schema

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCanonicalSortsValidatesAndRejectsBadMetrics(t *testing.T) {
	good := []Metric{
		{Name: "b_total", Kind: "counter", Labels: map[string][]string{"x": {"z", "a"}}},
		{Name: "a_seconds", Kind: "histogram", Buckets: []float64{0.1, 1}},
	}
	data, err := Canonical(good)
	if err != nil || strings.Index(string(data), "a_seconds") > strings.Index(string(data), "b_total") || strings.Index(string(data), `"a",`) > strings.Index(string(data), `"z"`) {
		t.Fatalf("canonical = %s, %v", data, err)
	}
	for name, bad := range map[string][]Metric{
		"duplicate":        {{Name: "x", Kind: "gauge"}, {Name: "x", Kind: "gauge"}},
		"bad kind":         {{Name: "x", Kind: "summary"}},
		"no buckets":       {{Name: "x", Kind: "histogram"}},
		"unsorted buckets": {{Name: "x", Kind: "histogram", Buckets: []float64{1, 0.5}}},
		"stray buckets":    {{Name: "x", Kind: "gauge", Buckets: []float64{1}}},
		"unbounded label":  {{Name: "x", Kind: "gauge", Labels: map[string][]string{"l": nil}}},
		"duplicate value":  {{Name: "x", Kind: "gauge", Labels: map[string][]string{"l": {"a", "a"}}}},
	} {
		if _, err := Canonical(bad); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestVerifyPrometheusChecksNamesLabelsBucketsAndCompleteness(t *testing.T) {
	document := Document{Metrics: []Metric{
		{Name: "req_total", Kind: "counter", Labels: map[string][]string{"outcome": {"ok"}}},
		{Name: "lat_seconds", Kind: "histogram", Buckets: []float64{0.5}},
	}}
	body := `req_total{outcome="ok"} 1
lat_seconds_bucket{le="0.5"} 1
lat_seconds_bucket{le="+Inf"} 1
lat_seconds_sum 0.2
lat_seconds_count 1`
	if err := VerifyPrometheus(document, body); err != nil {
		t.Fatal(err)
	}
	for name, bad := range map[string]string{
		"undocumented metric": body + "\nother 1",
		"undocumented label":  strings.Replace(body, `outcome="ok"`, `outcome="boom"`, 1),
		"undocumented bucket": strings.Replace(body, `le="0.5"`, `le="0.7"`, 1),
		"missing series":      strings.Replace(body, "lat_seconds_count 1", "", 1),
	} {
		if err := VerifyPrometheus(document, strings.TrimSpace(bad)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestRunWritesThenDetectsStaleDocument(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metrics.json")
	build := func() ([]Metric, error) { return []Metric{{Name: "a_total", Kind: "counter"}}, nil }
	var out bytes.Buffer
	if err := Run([]string{"-write", path}, &out, build, nil); err != nil {
		t.Fatal(err)
	}
	if err := Run([]string{path}, &out, build, func(Document) error { return nil }); err != nil {
		t.Fatalf("current document reported stale: %v", err)
	}
	if err := os.WriteFile(path, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Run([]string{path}, &out, build, nil); err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("stale document error = %v", err)
	}
}
