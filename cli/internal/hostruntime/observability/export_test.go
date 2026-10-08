package observability

import "testing"

func TestExportSnapshotsKeepCumulativeValuesAndFixedDimensions(t *testing.T) {
	r, err := NewRegistry(DefaultDescriptors())
	if err != nil {
		t.Fatal(err)
	}
	for _, result := range []string{"failed", "connected"} {
		if err = r.Record("paperboat_runtime_connector_retries_total", 1, map[string]string{"transport": "quic", "result": result}); err != nil {
			t.Fatal(err)
		}
	}
	if err = r.Record("paperboat_runtime_serve_latency_seconds", 2, map[string]string{"stage": "readiness", "owner": "foreground", "result": "ok"}); err != nil {
		t.Fatal(err)
	}
	samples := r.exportSamples()
	var failed, recovered, sum, count bool
	for _, s := range samples {
		switch s.Name {
		case "paperboat_runtime_connector_retries_total_snapshot":
			failed = failed || s.Labels["result"] == "failed"
			recovered = recovered || s.Labels["result"] == "connected"
			if s.Value != 1 {
				t.Fatal("counter changed")
			}
		case "paperboat_runtime_serve_latency_seconds_sum_snapshot":
			sum = s.Value == 2
		case "paperboat_runtime_serve_latency_seconds_count_snapshot":
			count = s.Value == 1
		}
	}
	if !failed || !recovered || !sum || !count {
		t.Fatal("failure/recovery or histogram evidence missing")
	}
	custom, _ := NewRegistry([]Descriptor{{Name: "paperboat_runtime_readiness", Kind: Gauge, Labels: map[string]map[string]bool{"private": {"secret": true}}}})
	if err = custom.Record("paperboat_runtime_readiness", 1, map[string]string{"private": "secret"}); err != nil {
		t.Fatal(err)
	}
	if len(custom.exportSamples()) != 0 {
		t.Fatal("custom private labels escaped built-in schema")
	}
}
