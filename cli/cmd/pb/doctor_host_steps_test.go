package main

import (
	"testing"

	doctorpkg "github.com/pinksaucepasta/paperboat/internal/doctor"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/health"
)

// A failed optional host component degrades only that item (pbmac2: preview_recovery could not set the
// system proxy as a standard user, and doctor called the whole machine unhealthy). A dimension that is down
// still fails local_state.
func TestDoctorHostStepsWarnsForADegradedComponentAndFailsWhenDown(t *testing.T) {
	degraded := health.Alert{Dimension: health.DimensionOrigin, Status: health.StatusDegraded, Code: "start_failed",
		Summary: "Host component preview_recovery failed to start: private proxy configuration failed", RepairAction: "Restart the Paperboat host service, then run pb doctor again."}
	check, ok := doctorHostStepsCheck([]health.Alert{degraded})
	if !ok || check.Status != doctorpkg.StatusWarning || check.Recovery == "" {
		t.Fatalf("degraded optional component: %+v", check)
	}
	down := degraded
	down.Status = health.StatusDown
	if check, _ := doctorHostStepsCheck([]health.Alert{degraded, down}); check.Status != doctorpkg.StatusFail {
		t.Fatalf("a down dimension must fail local_state: %+v", check)
	}
}
