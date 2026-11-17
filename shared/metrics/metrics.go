package metrics

import (
	"encoding/json"
	"math"
	"sort"
	"strings"
	"sync"

	"github.com/pinksaucepasta/paperboat/shared/telemetry/safe"
)

type Kind string

const (
	Counter   Kind = "counter"
	Gauge     Kind = "gauge"
	Histogram Kind = "histogram"
)

type LabelDescriptor struct {
	Name          string   `json:"name"`
	AllowedValues []string `json:"allowed_values"`
}

type Descriptor struct {
	Name      string               `json:"name"`
	Kind      Kind                 `json:"kind"`
	Labels    []LabelDescriptor    `json:"labels"`
	Histogram *HistogramDescriptor `json:"histogram,omitempty"`
}

type HistogramDescriptor struct {
	Buckets []float64 `json:"buckets"`
}

type Labels map[string]string

type Label struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type Sample struct {
	Name    string            `json:"name"`
	Kind    Kind              `json:"kind"`
	Labels  []Label           `json:"labels"`
	Value   uint64            `json:"value,omitempty"`
	Count   uint64            `json:"count,omitempty"`
	Sum     float64           `json:"sum,omitempty"`
	Buckets []HistogramBucket `json:"buckets,omitempty"`
}

type HistogramBucket struct {
	UpperBound float64 `json:"upper_bound"`
	Count      uint64  `json:"count"`
}

type metricKey struct {
	name   string
	values string
}

type Metrics struct {
	mu          sync.RWMutex
	descriptors map[string]Descriptor
	values      map[metricKey]metricValue
	maxSeries   int
}

type metricValue struct {
	value   uint64
	count   uint64
	sum     float64
	buckets []uint64
}

// DefaultMaximumSeries bounds the live series of a Metrics when the caller passes no limit.
const DefaultMaximumSeries = 1024

// New returns a Metrics that accepts exactly descriptors and at most maxSeries
// live series (DefaultMaximumSeries when maxSeries <= 0).
func New(descriptors []Descriptor, maxSeries int) *Metrics {
	if maxSeries <= 0 {
		maxSeries = DefaultMaximumSeries
	}
	byName := make(map[string]Descriptor, len(descriptors))
	for _, descriptor := range descriptors {
		byName[descriptor.Name] = cloneDescriptor(descriptor)
	}
	return &Metrics{descriptors: byName, values: make(map[metricKey]metricValue), maxSeries: maxSeries}
}

// Clone returns an independent copy of descriptors.
func Clone(descriptors []Descriptor) []Descriptor {
	result := make([]Descriptor, len(descriptors))
	for index, descriptor := range descriptors {
		result[index] = cloneDescriptor(descriptor)
	}
	return result
}

// MaximumLabelValues bounds a label whose values are added at run time.
const MaximumLabelValues = 1024

// AllowLabelValue makes value acceptable for a label whose values are registered names (route templates) the
// process itself defines, not anything a request carries. It does nothing when the value is already allowed and
// fails once the label holds MaximumLabelValues.
func (m *Metrics) AllowLabelValue(name, label, value string) error {
	if m == nil {
		return safe.NewError(safe.ErrorClosed, "record metric")
	}
	m.mu.RLock()
	known, err := m.labelValueAllowed(name, label, value)
	m.mu.RUnlock()
	if known || err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	descriptor := m.descriptors[name]
	// Copy on write: Snapshot and callers of prepare read the labels they were handed without the lock.
	labels := append([]LabelDescriptor(nil), descriptor.Labels...)
	for index := range labels {
		if labels[index].Name != label {
			continue
		}
		if containsString(labels[index].AllowedValues, value) {
			return nil
		}
		if len(labels[index].AllowedValues) >= MaximumLabelValues {
			return safe.NewError(safe.ErrorMetricCardinality, "record metric")
		}
		labels[index].AllowedValues = append(append([]string(nil), labels[index].AllowedValues...), value)
	}
	descriptor.Labels = labels
	m.descriptors[name] = descriptor
	return nil
}

// labelValueAllowed reports whether value is already allowed; the error names an unknown metric or label.
// Callers hold m.mu.
func (m *Metrics) labelValueAllowed(name, label, value string) (bool, error) {
	descriptor, ok := m.descriptors[name]
	if !ok {
		return false, safe.NewError(safe.ErrorUnknownMetric, "record metric")
	}
	for _, candidate := range descriptor.Labels {
		if candidate.Name == label {
			return containsString(candidate.AllowedValues, value), nil
		}
	}
	return false, safe.NewError(safe.ErrorUnknownLabel, "record metric")
}

func (m *Metrics) AddCounter(name string, labels Labels, delta uint64) error {
	return m.mutate(name, labels, Counter, delta, true)
}

func (m *Metrics) IncCounter(name string, labels Labels) error {
	return m.AddCounter(name, labels, 1)
}

func (m *Metrics) SetGauge(name string, labels Labels, value uint64) error {
	return m.mutate(name, labels, Gauge, value, false)
}

func (m *Metrics) ObserveHistogram(name string, labels Labels, observation float64) error {
	if math.IsNaN(observation) || math.IsInf(observation, 0) || observation < 0 {
		return safe.NewError(safe.ErrorInvalidObservation, "record metric")
	}
	descriptor, key, err := m.prepare(name, labels, Histogram)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	value, exists := m.values[key]
	if !exists {
		if len(m.values) >= m.maxSeries {
			return safe.NewError(safe.ErrorMetricCardinality, "record metric")
		}
		value.buckets = make([]uint64, len(descriptor.Histogram.Buckets))
	}
	if value.count == ^uint64(0) {
		return safe.NewError(safe.ErrorMetricOverflow, "record metric")
	}
	newSum := value.sum + observation
	if math.IsInf(newSum, 0) {
		return safe.NewError(safe.ErrorMetricOverflow, "record metric")
	}
	value.count++
	value.sum = newSum
	for index, upper := range descriptor.Histogram.Buckets {
		if observation <= upper {
			if value.buckets[index] == ^uint64(0) {
				return safe.NewError(safe.ErrorMetricOverflow, "record metric")
			}
			value.buckets[index]++
		}
	}
	m.values[key] = value
	return nil
}

// RaiseGauge sets the gauge to value when value is higher than the current
// reading (a high-water mark such as a maximum latency).
func (m *Metrics) RaiseGauge(name string, labels Labels, value uint64) error {
	_, key, err := m.prepare(name, labels, Gauge)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	current, exists := m.values[key]
	if !exists && len(m.values) >= m.maxSeries {
		return safe.NewError(safe.ErrorMetricCardinality, "record metric")
	}
	if !exists || value > current.value {
		current.value = value
		m.values[key] = current
	}
	return nil
}

func (m *Metrics) mutate(name string, labels Labels, kind Kind, value uint64, add bool) error {
	_, key, err := m.prepare(name, labels, kind)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	current, exists := m.values[key]
	if !exists && len(m.values) >= m.maxSeries {
		return safe.NewError(safe.ErrorMetricCardinality, "record metric")
	}
	if add {
		if ^uint64(0)-current.value < value {
			return safe.NewError(safe.ErrorMetricOverflow, "record metric")
		}
		current.value += value
	} else {
		current.value = value
	}
	m.values[key] = current
	return nil
}

func (m *Metrics) prepare(name string, labels Labels, kind Kind) (Descriptor, metricKey, error) {
	if m == nil {
		return Descriptor{}, metricKey{}, safe.NewError(safe.ErrorClosed, "record metric")
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	descriptor, ok := m.descriptors[name]
	if !ok {
		return Descriptor{}, metricKey{}, safe.NewError(safe.ErrorUnknownMetric, "record metric")
	}
	if descriptor.Kind != kind {
		return Descriptor{}, metricKey{}, safe.NewError(safe.ErrorMetricKindMismatch, "record metric")
	}
	key, err := validateMetricLabels(descriptor, labels)
	return descriptor, key, err
}

func (m *Metrics) Snapshot() []Sample {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	keys := make([]metricKey, 0, len(m.values))
	values := make(map[metricKey]metricValue, len(m.values))
	descriptors := make(map[string]Descriptor, len(m.descriptors))
	for key, value := range m.values {
		keys = append(keys, key)
		value.buckets = append([]uint64(nil), value.buckets...)
		values[key] = value
	}
	for name, descriptor := range m.descriptors {
		descriptors[name] = descriptor
	}
	m.mu.RUnlock()
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].name != keys[j].name {
			return keys[i].name < keys[j].name
		}
		return keys[i].values < keys[j].values
	})
	result := make([]Sample, 0, len(keys))
	for _, key := range keys {
		descriptor := descriptors[key.name]
		valuesForLabels := strings.Split(key.values, "\x1f")
		labels := make([]Label, len(descriptor.Labels))
		for index, label := range descriptor.Labels {
			labels[index] = Label{Name: label.Name, Value: valuesForLabels[index]}
		}
		value := values[key]
		sample := Sample{Name: key.name, Kind: descriptor.Kind, Labels: labels, Value: value.value, Count: value.count, Sum: value.sum}
		if descriptor.Kind == Histogram {
			sample.Buckets = make([]HistogramBucket, len(descriptor.Histogram.Buckets))
			for index, upper := range descriptor.Histogram.Buckets {
				sample.Buckets[index] = HistogramBucket{UpperBound: upper, Count: value.buckets[index]}
			}
		}
		result = append(result, sample)
	}
	return result
}

func (m *Metrics) JSON() ([]byte, error) { return json.Marshal(m.Snapshot()) }

func validateMetricLabels(descriptor Descriptor, labels Labels) (metricKey, error) {
	values := make([]string, len(descriptor.Labels))
	for index, label := range descriptor.Labels {
		value, ok := labels[label.Name]
		if !ok {
			return metricKey{}, safe.NewError(safe.ErrorMissingLabel, "record metric")
		}
		if !containsString(label.AllowedValues, value) {
			return metricKey{}, safe.NewError(safe.ErrorLabelValueRejected, "record metric")
		}
		values[index] = value
	}
	for name := range labels {
		if !descriptorHasLabel(descriptor, name) {
			return metricKey{}, safe.NewError(safe.ErrorUnknownLabel, "record metric")
		}
	}
	return metricKey{name: descriptor.Name, values: strings.Join(values, "\x1f")}, nil
}

func descriptorHasLabel(descriptor Descriptor, name string) bool {
	for _, label := range descriptor.Labels {
		if label.Name == name {
			return true
		}
	}
	return false
}

func containsString(values []string, candidate string) bool {
	for _, value := range values {
		if value == candidate {
			return true
		}
	}
	return false
}

func cloneDescriptor(descriptor Descriptor) Descriptor {
	result := descriptor
	result.Labels = make([]LabelDescriptor, len(descriptor.Labels))
	for index, label := range descriptor.Labels {
		result.Labels[index] = LabelDescriptor{Name: label.Name, AllowedValues: append([]string(nil), label.AllowedValues...)}
	}
	if descriptor.Histogram != nil {
		result.Histogram = &HistogramDescriptor{Buckets: append([]float64(nil), descriptor.Histogram.Buckets...)}
	}
	return result
}
