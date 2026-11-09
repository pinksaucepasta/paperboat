package main

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// withDoctorRepair runs --repair as Windows does: the privileged repair only for a paired installation.
func withDoctorRepair(t *testing.T, enrolled bool, repair func(context.Context) error) {
	t.Helper()
	previous := doctorPrivilegedRepair
	t.Cleanup(func() { doctorPrivilegedRepair = previous })
	doctorPrivilegedRepair = func(ctx context.Context) error {
		return repairEnrolled(ctx, func() (bool, error) { return enrolled, nil }, repair)
	}
}

// Windows Sandbox, installed but not paired: `pb doctor --repair` printed only "Paperboat host runtime command
// failed" (the elevated repair refuses an installation that awaits enrollment). Nothing privileged exists to
// repair there: no administrator approval is asked for and the normal report follows, as on Linux.
func TestDoctorRepairOnAnUnpairedComputerAsksForNoApprovalAndPrintsTheReport(t *testing.T) {
	repairs := 0
	withDoctorRepair(t, false, func(context.Context) error { repairs++; return nil })
	checkDoctorOwnerSocketJSON(t, "doctor", "--repair", "--json") // one JSON document: the normal report
	if repairs != 0 {
		t.Fatalf("an unpaired computer was repaired %d times", repairs)
	}
}

// Paired: the repair printed only the Windows dependency table and never ran the checks its help promises
// ("... then check"). It repairs once, then prints the same report as plain doctor with its exit code.
func TestDoctorRepairOnAPairedComputerRepairsOnceThenPrintsTheReport(t *testing.T) {
	repairs := 0
	withDoctorRepair(t, true, func(context.Context) error { repairs++; return nil })
	checkDoctorOwnerSocketJSON(t, "doctor", "--repair", "--json")
	if repairs != 1 {
		t.Fatalf("repairs = %d, want 1", repairs)
	}
}

// A repair that did not finish is the command's failure, named, as a failed daemon restart is on Linux: no report
// is printed over an installation the repair may have left half restored.
func TestDoctorRepairFailureIsNamedAndNoReportFollows(t *testing.T) {
	withDoctorRepair(t, true, func(context.Context) error { return errors.New("the administrator prompt was declined") })
	code, stdout, stderr := doctorWithoutADaemon(t, "windows", "doctor", "--repair")
	if code == 0 || stdout != "" || !strings.Contains(stderr, "could not repair the installation") || !strings.Contains(stderr, "`pb doctor --repair`") ||
		strings.Contains(stderr, "could not finish the command") || strings.Contains(stderr, "declined") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
}
