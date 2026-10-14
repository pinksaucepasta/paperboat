package supportreport

import (
	"github.com/getsentry/sentry-go"
	"github.com/getsentry/sentry-go/attribute"
	"testing"
)

func TestFleetValidationAndTrustedProjection(t *testing.T) {
	for _, pair := range [][2]string{{"", "slot-01"}, {"mumbai", ""}, {"mumbai", "slot-00"}, {"mumbai", "slot-65"}, {"private.example.com", "slot-01"}} {
		if _, err := New(&memoryStore{}, Config{Region: pair[0], Instance: pair[1]}); err == nil {
			t.Fatalf("accepted invalid fleet %v", pair)
		}
	}
	s, err := New(&memoryStore{}, Config{Region: "mumbai", Instance: "slot-64"})
	if err != nil {
		t.Fatal(err)
	}
	attrs := map[string]attribute.Value{"operation": attribute.StringValue("http_auth"), "outcome": attribute.StringValue("success"), "region": attribute.StringValue("SECRET"), "instance": attribute.StringValue("SECRET")}
	log := s.cleanLog(&sentry.Log{Body: "paperboat.operation", Attributes: attrs})
	metric := s.cleanMetric(&sentry.Metric{Name: "paperboat.operation.count", Value: sentry.Int64MetricValue(1), Attributes: attrs})
	trace := s.cleanTransaction(&sentry.Event{Transaction: "http_auth", Tags: map[string]string{"region": "SECRET"}}, nil)
	for _, got := range []string{log.Attributes["region"].AsString(), metric.Attributes["region"].AsString(), trace.Tags["region"]} {
		if got != "mumbai" {
			t.Fatalf("untrusted region %q", got)
		}
	}
	for _, got := range []string{log.Attributes["instance"].AsString(), metric.Attributes["instance"].AsString(), trace.Tags["instance"]} {
		if got != "slot-64" {
			t.Fatalf("untrusted slot %q", got)
		}
	}
}

func TestBackupHealthMetricStatesAreFinite(t *testing.T) {
	s, err := New(&memoryStore{}, Config{})
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []float64{0, 1, 2, 3, 4, 5, 6} {
		if s.cleanMetric(&sentry.Metric{Name: "paperboat.backup.state", Value: sentry.Float64MetricValue(value)}) == nil {
			t.Fatalf("state %v rejected", value)
		}
	}
	for _, value := range []float64{-1, .5, 7} {
		if s.cleanMetric(&sentry.Metric{Name: "paperboat.restore.state", Value: sentry.Float64MetricValue(value)}) != nil {
			t.Fatalf("state %v accepted", value)
		}
	}
}
