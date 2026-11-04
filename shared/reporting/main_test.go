package reporting

import (
	"os"
	"testing"

	"github.com/getsentry/sentry-go"
)

// The shared tests exercise a service's registered vocabulary the way relay/reportcause registers it.
func TestMain(m *testing.M) {
	gauge := sentry.MetricTypeGauge
	outcomes := map[string][]string{"outcome": {"success", "rejected"}}
	Register(Extension{
		Definitions: map[string]Definition{
			"selfhost_command":     {Operation: "service_lifecycle", Stage: "selfhost", Name: "selfhost_failed", Code: "selfhost_command_failed"},
			"selfhost_child":       {Operation: "service_lifecycle", Stage: "selfhost", Name: "selfhost_failed", Code: "selfhost_child_failed"},
			"selfhost_certificate": {Operation: "dependency_health", Stage: "selfhost", Name: "selfhost_failed", Code: "selfhost_certificate_failed"},
			"selfhost_claim":       {Operation: "service_lifecycle", Stage: "selfhost", Name: "selfhost_failed", Code: "selfhost_claim_failed"},
		},
		Codes:  []string{"selfhost_command_failed", "selfhost_child_failed", "selfhost_certificate_failed", "selfhost_claim_failed"},
		Stages: map[string]string{"selfhost": "selfhost_failed"},
		Metrics: []Metric{
			{Name: "paperboat_relay_sessions", Kind: gauge}, {Name: "paperboat_peer_relay_allocations", Kind: gauge}, {Name: "paperboat_peer_relay_admissions_total_snapshot", Kind: gauge, Labels: outcomes}, {Name: "paperboat_peer_relay_packets_total_snapshot", Kind: gauge}, {Name: "paperboat_relay_capacity_limit", Kind: gauge},
			{Name: "paperboat_relay_admissions_total_snapshot", Kind: gauge, Labels: outcomes},
			{Name: "paperboat_relay_packets_total_snapshot", Kind: gauge, Labels: map[string][]string{"outcome": {"forwarded", "dropped"}}},
		},
	})
	os.Exit(m.Run())
}
