package doctor

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// C020 (round 9, Linux test machine): `pb doctor` right after `pb setup` said "degraded" for a daemon that was only
// starting. A pending check makes the report "starting", not degraded; a real warning or failure
// still decides the overall.
func TestAStartingDaemonMakesTheReportStartingNotDegraded(t *testing.T) {
	pass := Check{Status: StatusPass}
	pending := Check{Status: StatusPending}
	for name, test := range map[string]struct {
		checks []Check
		want   string
	}{
		"all pass":               {[]Check{pass, pass}, "healthy"},
		"daemon starting":        {[]Check{pass, pending}, "starting"},
		"starting and a warning": {[]Check{pending, {Status: StatusWarning}}, "degraded"},
		"warning then starting":  {[]Check{{Status: StatusWarning}, pending}, "degraded"},
		"starting and a failure": {[]Check{pending, {Status: StatusFail}}, "unhealthy"},
	} {
		if got := overall(test.checks); got != test.want {
			t.Errorf("%s: overall=%q, want %q", name, got, test.want)
		}
	}
}

func TestReportProjectsHomeRelayRegionOnly(t *testing.T) {
	report := Report{Schema: SchemaV1, CorrelationID: "corr-1", CheckedAt: time.Now(), Overall: "healthy", Checks: []Check{{Category: "daemon", Code: "a", Status: StatusPass, Summary: "ok"}}, HomeRelay: &HomeRelay{Region: "india"}}
	encoded, err := json.Marshal(report)
	if err != nil || !strings.Contains(string(encoded), `"home_relay":{"region":"india"}`) || strings.Contains(string(encoded), "node_id") {
		t.Fatalf("report = %s (%v)", encoded, err)
	}
	if err := report.Validate(); err != nil {
		t.Fatalf("a region-only home relay is rejected: %v", err)
	}
	if encoded, _ = json.Marshal(Report{Schema: SchemaV1}); strings.Contains(string(encoded), "home_relay") {
		t.Fatalf("a report without a home relay names one: %s", encoded)
	}
}
