package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	doctorpkg "github.com/pinksaucepasta/paperboat/internal/doctor"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/health"
)

// A host runtime step that failed (a degraded typed health dimension) makes pb doctor unhealthy, naming the
// step, its cause and the repair; dimensions not observed yet are not failures; after recovery doctor is
// healthy again.
func TestDoctorNamesAFailingHostStepUntilItRecovers(t *testing.T) {
	tracker, err := health.NewHealthTracker(nil)
	if err != nil {
		t.Fatal(err)
	}
	run := func() (doctorpkg.Report, string) {
		t.Helper()
		steps := failingHostSteps(tracker.Snapshot())
		probe := doctorpkg.Probe{Code: "local_state", Run: func(context.Context) doctorpkg.Check {
			if check, failing := doctorHostStepsCheck(steps); failing {
				return check
			}
			return doctorpkg.Check{Category: "local", Code: "local_state", Status: doctorpkg.StatusPass, Summary: "Local Paperboat state and identity are valid."}
		}}
		report, err := doctorpkg.Run(t.Context(), doctorpkg.Config{Timeout: time.Second, ProbeTimeout: time.Second, Clock: time.Now,
			Correlation: func() (string, error) { return "support_test", nil }}, nil, []doctorpkg.Probe{probe})
		if err != nil {
			t.Fatal(err)
		}
		var output bytes.Buffer
		writeDoctorReport(&output, report)
		return report, output.String()
	}
	if report, output := run(); report.Overall != "healthy" {
		t.Fatalf("unobserved dimensions failed doctor:\n%s", output)
	}
	update := health.HealthUpdate{Dimension: health.DimensionEdge, Status: health.StatusDegraded, Code: "start_failed",
		Summary: "Host component peer_transport failed to start: peer registration refused", RepairAction: "Paperboat retries this on its own.",
		Retry: health.RetryScheduled, NextRetryAt: time.Now().Add(time.Minute)}
	if err := tracker.Update(update); err != nil {
		t.Fatal(err)
	}
	report, output := run()
	if report.Overall != "unhealthy" || !strings.Contains(output, "local_state: fail: The host runtime reports a failing step (edge): Host component peer_transport failed to start: peer registration refused") ||
		!strings.Contains(output, "Recovery: Paperboat retries this on its own.") {
		t.Fatalf("failing host step: overall=%s output:\n%s", report.Overall, output)
	}
	if err := tracker.Update(health.HealthUpdate{Dimension: health.DimensionEdge, Status: health.StatusReady, Code: "ready", Summary: "Host component peer_transport is running.", RepairAction: "No action is required.", Retry: health.RetryNone}); err != nil {
		t.Fatal(err)
	}
	if report, output = run(); report.Overall != "healthy" || strings.Contains(output, "failing step") {
		t.Fatalf("recovered host step: overall=%s output:\n%s", report.Overall, output)
	}
}
