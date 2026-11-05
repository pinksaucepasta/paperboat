package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	doctorpkg "github.com/pinksaucepasta/paperboat/internal/doctor"
	"github.com/pinksaucepasta/paperboat/internal/localapi"
)

// A failing daemon step makes pb doctor unhealthy and names the step, its cause and the action; once the step
// recovers the same run is healthy and lists nothing about it.
func TestDoctorNamesAFailingDaemonStepUntilItRecovers(t *testing.T) {
	run := func(items []localapi.HealthItem) (doctorpkg.Report, string) {
		t.Helper()
		pass := doctorpkg.Probe{Code: "daemon", Run: func(context.Context) doctorpkg.Check {
			return doctorpkg.Check{Category: "local", Code: "daemon", Status: doctorpkg.StatusPass, Summary: "The local daemon is ready."}
		}}
		report, err := doctorpkg.Run(t.Context(), doctorpkg.Config{Timeout: time.Second, ProbeTimeout: time.Second, Clock: time.Now,
			Correlation: func() (string, error) { return "support_test", nil }}, nil, append([]doctorpkg.Probe{pass}, doctorDaemonHealthProbes(items)...))
		if err != nil {
			t.Fatal(err)
		}
		var output bytes.Buffer
		writeDoctorReport(&output, report)
		return report, output.String()
	}
	failing := []localapi.HealthItem{
		{Code: "register_network", Severity: "error", Title: "Registering this computer with the Paperboat network failed: dial tcp: connection refused; retrying in 4s", Recovery: "Paperboat retries this on its own.", ETag: "register_network"},
		{Code: "relay_failover", Severity: "info", Title: "The relay failed over", Recovery: "No action is needed.", ETag: "relay_failover"},
	}
	report, output := run(failing)
	if report.Overall != "unhealthy" || !strings.Contains(output, "daemon:register_network: fail: Registering this computer") ||
		!strings.Contains(output, "connection refused") || !strings.Contains(output, "Recovery: Paperboat retries this on its own.") {
		t.Fatalf("failing step: overall=%s output:\n%s", report.Overall, output)
	}
	if strings.Contains(output, "relay_failover") {
		t.Fatalf("an info item became a doctor check:\n%s", output)
	}
	report, output = run(failing[1:])
	if report.Overall != "healthy" || strings.Contains(output, "daemon:register_network") {
		t.Fatalf("recovered step: overall=%s output:\n%s", report.Overall, output)
	}
}
