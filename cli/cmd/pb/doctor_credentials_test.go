package main

import (
	"strings"
	"testing"
	"time"
)

// lanep-r26b logged machine_control 401s all day while doctor said healthy (Lane P, 2026-10-26).
func TestDoctorCountsRecentMachineCredentialRefusals(t *testing.T) {
	now := time.Now().UTC()
	line := func(at time.Time, code, operation, status string) string {
		return `{"schema":"paperboat.diagnostic-event/v1","at":"` + at.Format(time.RFC3339Nano) + `","category":"command","code":"` + code + `","severity":"error","fields":{"operation":"` + operation + `","http_status":"` + status + `"}}`
	}
	events := strings.Join([]string{
		line(now.Add(-2*time.Hour), "control_request_failed", "machine_control", "401"),
		line(now.Add(-30*time.Minute), "control_request_failed", "machine_control", "401"),
		line(now.Add(-20*time.Minute), "control_request_failed", "machine_control", "401"),
		line(now.Add(-10*time.Minute), "control_request_failed", "machine_control", "401"),
		line(now.Add(-5*time.Minute), "control_request_failed", "preview_attachment", "401"),
		line(now.Add(-5*time.Minute), "control_request_failed", "machine_control", "503"),
		"not json",
	}, "\n")
	if got := countCredentialRefusals([]byte(events), now.Add(-credentialRefusalWindow)); got != 3 {
		t.Fatalf("refusals in the last hour = %d, want 3", got)
	}
}
