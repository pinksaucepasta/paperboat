package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat-tunnel/internal/node"
	"github.com/pinksaucepasta/paperboat-tunnel/internal/observability"
	edgetelemetry "github.com/pinksaucepasta/paperboat-tunnel/internal/telemetry"
	"github.com/pinksaucepasta/paperboat-tunnel/internal/usage"
)

const schemaVersion = 1

type document struct {
	SchemaVersion int      `json:"schema_version"`
	Metrics       []metric `json:"metrics"`
}

type metric struct {
	Name    string              `json:"name"`
	Kind    string              `json:"kind"`
	Labels  map[string][]string `json:"labels,omitempty"`
	Buckets []float64           `json:"buckets,omitempty"`
}

func main() {
	write := flag.Bool("write", false, "write the canonical metric schema")
	flag.Parse()
	if flag.NArg() != 1 {
		fatalf("usage: metric-schema [-write] DOCUMENT")
	}
	data, err := canonicalDocument()
	if err != nil {
		fatalf("metric schema: %v", err)
	}
	path := flag.Arg(0)
	if *write {
		if err := os.WriteFile(path, data, 0o644); err != nil {
			fatalf("write metric schema: %v", err)
		}
		return
	}
	current, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(current, data) {
		fatalf("%s is stale; run make metrics-generate", path)
	}
	if err := verifyHandler(); err != nil {
		fatalf("metric handler: %v", err)
	}
}

func canonicalDocument() ([]byte, error) {
	descriptors := observability.MetricDescriptors()
	metrics := make([]metric, 0, len(descriptors))
	for index, descriptor := range descriptors {
		if descriptor.Name == "" || descriptor.Kind != "counter" && descriptor.Kind != "gauge" && descriptor.Kind != "histogram" {
			return nil, fmt.Errorf("invalid descriptor %+v", descriptor)
		}
		if descriptor.Kind == "histogram" {
			if len(descriptor.Buckets) == 0 {
				return nil, fmt.Errorf("histogram %q has no buckets", descriptor.Name)
			}
			for bucketIndex, bucket := range descriptor.Buckets {
				if bucket <= 0 || math.IsNaN(bucket) || math.IsInf(bucket, 0) || bucketIndex > 0 && descriptor.Buckets[bucketIndex-1] >= bucket {
					return nil, fmt.Errorf("histogram %q has invalid bucket %v", descriptor.Name, bucket)
				}
			}
		} else if len(descriptor.Buckets) != 0 {
			return nil, fmt.Errorf("non-histogram %q has buckets", descriptor.Name)
		}
		if index > 0 && descriptors[index-1].Name >= descriptor.Name {
			return nil, fmt.Errorf("descriptors are not uniquely sorted at %q", descriptor.Name)
		}
		for label, values := range descriptor.Labels {
			if label == "" || len(values) == 0 || !sort.StringsAreSorted(values) {
				return nil, fmt.Errorf("metric %q label %q is not bounded and sorted", descriptor.Name, label)
			}
			for valueIndex := 1; valueIndex < len(values); valueIndex++ {
				if values[valueIndex-1] == values[valueIndex] {
					return nil, fmt.Errorf("metric %q label %q contains duplicate values", descriptor.Name, label)
				}
			}
		}
		metrics = append(metrics, metric{Name: descriptor.Name, Kind: descriptor.Kind, Labels: descriptor.Labels, Buckets: descriptor.Buckets})
	}
	data, err := json.MarshalIndent(document{SchemaVersion: schemaVersion, Metrics: metrics}, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

func verifyHandler() error {
	now := time.Unix(1_800_000_000, 0).UTC()
	schemaData, err := canonicalDocument()
	if err != nil {
		return err
	}
	typedMetrics := edgetelemetry.NewMetrics()
	for _, descriptor := range edgetelemetry.MetricDescriptors() {
		labels := make(edgetelemetry.MetricLabels, len(descriptor.Labels))
		for _, label := range descriptor.Labels {
			if len(label.AllowedValues) == 0 {
				return fmt.Errorf("metric %q label %q has no allowed values", descriptor.Name, label.Name)
			}
			labels[label.Name] = label.AllowedValues[0]
		}
		var err error
		switch descriptor.Kind {
		case edgetelemetry.MetricCounter:
			err = typedMetrics.AddCounter(descriptor.Name, labels, 1)
		case edgetelemetry.MetricGauge:
			err = typedMetrics.SetGauge(descriptor.Name, labels, 1)
		case edgetelemetry.MetricHistogram:
			if descriptor.Histogram == nil || len(descriptor.Histogram.Buckets) == 0 {
				return fmt.Errorf("histogram %q has no buckets", descriptor.Name)
			}
			err = typedMetrics.ObserveHistogram(descriptor.Name, labels, descriptor.Histogram.Buckets[0])
		default:
			return fmt.Errorf("metric %q has unsupported kind %q", descriptor.Name, descriptor.Kind)
		}
		if err != nil {
			return fmt.Errorf("populate metric %q: %w", descriptor.Name, err)
		}
	}
	handler, err := observability.NewHandler(observability.Sources{
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
	var schema document
	if err := json.Unmarshal(schemaData, &schema); err != nil {
		return fmt.Errorf("decode canonical schema: %w", err)
	}
	documented := make(map[string]metric, len(schema.Metrics))
	for _, descriptor := range schema.Metrics {
		documented[descriptor.Name] = descriptor
	}
	emitted := make(map[string]map[string]struct{})
	for _, line := range strings.Split(strings.TrimSpace(recorder.Body.String()), "\n") {
		token := strings.Fields(line)[0]
		name := token
		labels := map[string]string{}
		if index := strings.IndexByte(token, '{'); index >= 0 {
			name = token[:index]
			parsed, parseErr := parseLabels(token[index+1 : len(token)-1])
			if parseErr != nil {
				return fmt.Errorf("parse %s labels: %w", name, parseErr)
			}
			labels = parsed
		}
		descriptorName, series, ok := resolveMetricLine(name, documented)
		if !ok {
			return fmt.Errorf("handler emitted undocumented metric %q", name)
		}
		descriptor := documented[descriptorName]
		if err := validateLabels(descriptor, series, labels); err != nil {
			return err
		}
		if emitted[descriptorName] == nil {
			emitted[descriptorName] = make(map[string]struct{})
		}
		emitted[descriptorName][series] = struct{}{}
	}
	for name, descriptor := range documented {
		if descriptor.Kind != "histogram" {
			if _, ok := emitted[name]["value"]; !ok {
				return fmt.Errorf("documented metric %q was not emitted", name)
			}
			continue
		}
		for _, series := range []string{"bucket", "sum", "count"} {
			if _, ok := emitted[name][series]; !ok {
				return fmt.Errorf("documented histogram %q %s series was not emitted", name, series)
			}
		}
	}
	return nil
}

func resolveMetricLine(name string, documented map[string]metric) (descriptorName, series string, ok bool) {
	if _, exists := documented[name]; exists {
		return name, "value", true
	}
	for _, suffix := range []string{"_bucket", "_sum", "_count"} {
		if !strings.HasSuffix(name, suffix) {
			continue
		}
		base := strings.TrimSuffix(name, suffix)
		if descriptor, exists := documented[base]; exists && descriptor.Kind == "histogram" {
			return base, strings.TrimPrefix(suffix, "_"), true
		}
	}
	return "", "", false
}

func parseLabels(value string) (map[string]string, error) {
	result := map[string]string{}
	if value == "" {
		return result, nil
	}
	for _, item := range strings.Split(value, ",") {
		key, quoted, ok := strings.Cut(item, "=")
		if !ok || key == "" {
			return nil, fmt.Errorf("invalid label %q", item)
		}
		decoded, err := strconv.Unquote(quoted)
		if err != nil {
			return nil, err
		}
		if _, exists := result[key]; exists {
			return nil, fmt.Errorf("duplicate label %q", key)
		}
		result[key] = decoded
	}
	return result, nil
}

func validateLabels(descriptor metric, series string, labels map[string]string) error {
	baseLabels := labels
	if descriptor.Kind == "histogram" && series == "bucket" {
		le, ok := labels["le"]
		if !ok || !contains(histogramBucketValues(descriptor.Buckets), le) {
			return fmt.Errorf("histogram %q has undocumented bucket label %q", descriptor.Name, le)
		}
		baseLabels = make(map[string]string, len(labels)-1)
		for key, value := range labels {
			if key != "le" {
				baseLabels[key] = value
			}
		}
	}
	if len(baseLabels) != len(descriptor.Labels) {
		return fmt.Errorf("metric %q labels=%v want=%v", descriptor.Name, baseLabels, descriptor.Labels)
	}
	for label, value := range baseLabels {
		allowed, ok := descriptor.Labels[label]
		if !ok || !contains(allowed, value) {
			return fmt.Errorf("metric %q has undocumented label %s=%q", descriptor.Name, label, value)
		}
	}
	return nil
}

func histogramBucketValues(buckets []float64) []string {
	values := make([]string, len(buckets)+1)
	for index, bucket := range buckets {
		values[index] = strconv.FormatFloat(bucket, 'g', -1, 64)
	}
	values[len(buckets)] = "+Inf"
	return values
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func fatalf(format string, values ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", values...)
	os.Exit(1)
}
