// Package schema is the one metric-schema tool: it turns a binary's metric
// descriptors into the canonical docs/metrics.json document, checks that the
// committed document is current, and checks that a handler's Prometheus text
// exposes exactly the documented metrics with documented labels and buckets.
// Each module's tools/metric-schema/main.go only says where its descriptors and
// its handler are.
package schema

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/pinksaucepasta/paperboat/shared/metrics"
)

// Version is the schema_version of the document.
const Version = 1

// Metric is one documented metric.
type Metric struct {
	Name    string              `json:"name"`
	Kind    string              `json:"kind"`
	Labels  map[string][]string `json:"labels,omitempty"`
	Buckets []float64           `json:"buckets,omitempty"`
}

// Document is docs/metrics.json.
type Document struct {
	SchemaVersion int      `json:"schema_version"`
	Metrics       []Metric `json:"metrics"`
}

// FromDescriptors documents typed descriptors.
func FromDescriptors(descriptors []metrics.Descriptor) []Metric {
	result := make([]Metric, 0, len(descriptors))
	for _, descriptor := range descriptors {
		metric := Metric{Name: descriptor.Name, Kind: string(descriptor.Kind)}
		if len(descriptor.Labels) != 0 {
			metric.Labels = make(map[string][]string, len(descriptor.Labels))
			for _, label := range descriptor.Labels {
				metric.Labels[label.Name] = append([]string(nil), label.AllowedValues...)
			}
		}
		if descriptor.Histogram != nil {
			metric.Buckets = append([]float64(nil), descriptor.Histogram.Buckets...)
		}
		result = append(result, metric)
	}
	return result
}

// Canonical validates metrics and returns the canonical document: metrics sorted
// by name, label values sorted. A name may appear once, a histogram needs strictly
// increasing finite positive buckets, and every label needs a bounded value list.
func Canonical(metrics []Metric) ([]byte, error) {
	sorted := make([]Metric, len(metrics))
	for index, metric := range metrics {
		sorted[index] = metric
		sorted[index].Labels = make(map[string][]string, len(metric.Labels))
		for label, values := range metric.Labels {
			sorted[index].Labels[label] = append([]string(nil), values...)
			sort.Strings(sorted[index].Labels[label])
		}
		if len(sorted[index].Labels) == 0 {
			sorted[index].Labels = nil
		}
	}
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	for index, metric := range sorted {
		if metric.Name == "" || metric.Kind != "counter" && metric.Kind != "gauge" && metric.Kind != "histogram" {
			return nil, fmt.Errorf("invalid descriptor %+v", metric)
		}
		if index > 0 && sorted[index-1].Name == metric.Name {
			return nil, fmt.Errorf("duplicate metric %q", metric.Name)
		}
		if err := validateBuckets(metric); err != nil {
			return nil, err
		}
		for label, values := range metric.Labels {
			if label == "" || len(values) == 0 {
				return nil, fmt.Errorf("metric %q label %q is not bounded", metric.Name, label)
			}
			for valueIndex := 1; valueIndex < len(values); valueIndex++ {
				if values[valueIndex-1] == values[valueIndex] {
					return nil, fmt.Errorf("metric %q label %q contains duplicate values", metric.Name, label)
				}
			}
		}
	}
	data, err := json.MarshalIndent(Document{SchemaVersion: Version, Metrics: sorted}, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

func validateBuckets(metric Metric) error {
	if metric.Kind != "histogram" {
		if len(metric.Buckets) != 0 {
			return fmt.Errorf("non-histogram %q has buckets", metric.Name)
		}
		return nil
	}
	if len(metric.Buckets) == 0 {
		return fmt.Errorf("histogram %q has no buckets", metric.Name)
	}
	for index, bucket := range metric.Buckets {
		if bucket <= 0 || math.IsNaN(bucket) || math.IsInf(bucket, 0) || index > 0 && metric.Buckets[index-1] >= bucket {
			return fmt.Errorf("histogram %q has invalid bucket %v", metric.Name, bucket)
		}
	}
	return nil
}

// Run is a module's metric-schema main. build returns the module's descriptors;
// verify, when it is not nil, checks the module's handler against the document
// (usually with VerifyPrometheus). With -write it writes the document; otherwise it
// fails when the committed document is stale, then runs verify.
func Run(args []string, stderr io.Writer, build func() ([]Metric, error), verify func(Document) error) error {
	flags := flag.NewFlagSet("metric-schema", flag.ContinueOnError)
	flags.SetOutput(stderr)
	write := flags.Bool("write", false, "write the canonical metric schema")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 1 {
		return errors.New("usage: metric-schema [-write] DOCUMENT")
	}
	descriptors, err := build()
	if err != nil {
		return fmt.Errorf("metric schema could not be built: %w", err)
	}
	data, err := Canonical(descriptors)
	if err != nil {
		return fmt.Errorf("metric schema is invalid: %w", err)
	}
	path := flags.Arg(0)
	if *write {
		if err := os.WriteFile(path, data, 0o644); err != nil {
			return fmt.Errorf("metric schema document could not be written: %w", err)
		}
		return nil
	}
	current, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("metric schema document could not be read: %w", err)
	}
	if !bytes.Equal(current, data) {
		return fmt.Errorf("%s is stale; run make metrics-generate", path)
	}
	if verify == nil {
		return nil
	}
	var document Document
	if err := json.Unmarshal(data, &document); err != nil {
		return fmt.Errorf("decode canonical schema: %w", err)
	}
	if err := verify(document); err != nil {
		return fmt.Errorf("metric handler verification failed: %w", err)
	}
	return nil
}

// Main runs Run with the process arguments and exits non-zero on failure.
func Main(build func() ([]Metric, error), verify func(Document) error) {
	if err := Run(os.Args[1:], os.Stderr, build, verify); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		fmt.Fprintln(os.Stderr, "metric-schema:", err)
		os.Exit(1)
	}
}

// VerifyPrometheus checks a handler's Prometheus text against document: every line
// names a documented metric (histograms as _bucket, _sum and _count) with exactly
// its documented labels and values, and every documented metric is emitted.
func VerifyPrometheus(document Document, body string) error {
	documented := make(map[string]Metric, len(document.Metrics))
	for _, metric := range document.Metrics {
		documented[metric.Name] = metric
	}
	emitted := make(map[string]map[string]struct{})
	for _, line := range strings.Split(strings.TrimSpace(body), "\n") {
		token := strings.Fields(line)[0]
		name := token
		labels := map[string]string{}
		if index := strings.IndexByte(token, '{'); index >= 0 {
			name = token[:index]
			parsed, err := parseLabels(token[index+1 : len(token)-1])
			if err != nil {
				return fmt.Errorf("parse %s labels: %w", name, err)
			}
			labels = parsed
		}
		descriptorName, series, ok := resolveLine(name, documented)
		if !ok {
			return fmt.Errorf("handler emitted undocumented metric %q", name)
		}
		if err := validateLabels(documented[descriptorName], series, labels); err != nil {
			return err
		}
		if emitted[descriptorName] == nil {
			emitted[descriptorName] = make(map[string]struct{})
		}
		emitted[descriptorName][series] = struct{}{}
	}
	for name, metric := range documented {
		wanted := []string{"value"}
		if metric.Kind == "histogram" {
			wanted = []string{"bucket", "sum", "count"}
		}
		for _, series := range wanted {
			if _, ok := emitted[name][series]; !ok {
				return fmt.Errorf("documented metric %q %s series was not emitted", name, series)
			}
		}
	}
	return nil
}

func resolveLine(name string, documented map[string]Metric) (descriptorName, series string, ok bool) {
	if _, exists := documented[name]; exists {
		return name, "value", true
	}
	for _, suffix := range []string{"_bucket", "_sum", "_count"} {
		if !strings.HasSuffix(name, suffix) {
			continue
		}
		base := strings.TrimSuffix(name, suffix)
		if metric, exists := documented[base]; exists && metric.Kind == "histogram" {
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

func validateLabels(metric Metric, series string, labels map[string]string) error {
	base := labels
	if metric.Kind == "histogram" && series == "bucket" {
		le, ok := labels["le"]
		if !ok || !contains(bucketValues(metric.Buckets), le) {
			return fmt.Errorf("histogram %q has undocumented bucket label %q", metric.Name, le)
		}
		base = make(map[string]string, len(labels)-1)
		for key, value := range labels {
			if key != "le" {
				base[key] = value
			}
		}
	}
	if len(base) != len(metric.Labels) {
		return fmt.Errorf("metric %q labels=%v want=%v", metric.Name, base, metric.Labels)
	}
	for label, value := range base {
		allowed, ok := metric.Labels[label]
		if !ok || !contains(allowed, value) {
			return fmt.Errorf("metric %q has undocumented label %s=%q", metric.Name, label, value)
		}
	}
	return nil
}

func bucketValues(buckets []float64) []string {
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
