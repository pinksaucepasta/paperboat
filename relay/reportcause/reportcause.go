// Package reportcause classifies the relay's own typed errors for shared/reporting.
package reportcause

import (
	"github.com/getsentry/sentry-go"
	"github.com/pinksaucepasta/paperboat/relay/derpquic"
	"github.com/pinksaucepasta/paperboat/relay/nodelifecycle"
	"github.com/pinksaucepasta/paperboat/shared/reporting"
)

func init() {
	gauge := sentry.MetricTypeGauge
	outcomes := map[string][]string{"outcome": {"success", "rejected"}}
	reporting.Register(reporting.Extension{
		Definitions: map[string]reporting.Definition{
			"selfhost_command":     {Operation: "service_lifecycle", Stage: "selfhost", Name: "selfhost_failed", Code: "selfhost_command_failed"},
			"selfhost_child":       {Operation: "service_lifecycle", Stage: "selfhost", Name: "selfhost_failed", Code: "selfhost_child_failed"},
			"selfhost_certificate": {Operation: "dependency_health", Stage: "selfhost", Name: "selfhost_failed", Code: "selfhost_certificate_failed"},
			"selfhost_claim":       {Operation: "service_lifecycle", Stage: "selfhost", Name: "selfhost_failed", Code: "selfhost_claim_failed"},
		},
		Codes:    []string{"selfhost_command_failed", "selfhost_child_failed", "selfhost_certificate_failed", "selfhost_claim_failed"},
		Causes:   []string{"authority_unavailable", "authority_rejected", "authorization_rejected", "protocol_rejected", "capacity_exhausted", "authority_expired", "relay_closed", "bandwidth_quota_exhausted"},
		Stages:   map[string]string{"selfhost": "selfhost_failed"},
		Classify: classify,
		Metrics: []reporting.Metric{
			{Name: "paperboat_relay_sessions", Kind: gauge}, {Name: "paperboat_relay_capacity_limit", Kind: gauge},
			{Name: "paperboat_relay_admissions_total_snapshot", Kind: gauge, Labels: outcomes},
			{Name: "paperboat_relay_packets_total_snapshot", Kind: gauge, Labels: map[string][]string{"outcome": {"forwarded", "dropped"}}},
			{Name: "paperboat_peer_relay_allocations", Kind: gauge},
			{Name: "paperboat_peer_relay_admissions_total_snapshot", Kind: gauge, Labels: outcomes},
			{Name: "paperboat_peer_relay_packets_total_snapshot", Kind: gauge},
		},
	})
}

func classify(err error) (string, int) {
	switch {
	case reporting.SameError(err, nodelifecycle.ErrFenced):
		return "authority_rejected", 85
	case reporting.SameError(err, nodelifecycle.ErrControl):
		return "authority_unavailable", 40
	case reporting.SameError(err, derpquic.ErrAdmission):
		return "authorization_rejected", 83
	case reporting.SameError(err, derpquic.ErrProtocol):
		return "protocol_rejected", 83
	case reporting.SameError(err, derpquic.ErrOverload):
		return "capacity_exhausted", 83
	case reporting.SameError(err, derpquic.ErrExpired):
		return "authority_expired", 83
	case reporting.SameError(err, derpquic.ErrQuota):
		return "bandwidth_quota_exhausted", 83
	case reporting.SameError(err, derpquic.ErrClosed):
		return "relay_closed", 82
	}
	if typed, ok := err.(*derpquic.Error); ok {
		switch typed.Code {
		case 1:
			return "authorization_rejected", 83
		case 2:
			return "protocol_rejected", 83
		case 3:
			return "capacity_exhausted", 83
		case 4:
			return "authority_expired", 83
		case 5:
			return "bandwidth_quota_exhausted", 83
		}
	}
	return "", 0
}
