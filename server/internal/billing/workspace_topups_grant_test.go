package billing

import (
	"testing"
	"time"
)

// A support grant has no payer, so it cannot buy top-ups; the same workspace as a paid plan can.
func TestTopupsNeedAPaidPlanNotAGrant(t *testing.T) {
	now := time.Now().UTC()
	start, until := now.Add(-time.Hour), now.Add(24*time.Hour)
	paid := Workspace{ProductCode: "pro", CustomerID: "cus_1", PeriodStart: &start, PaidBenefitsUntil: &until}
	if !paidTopupWorkspace(paid, now) {
		t.Fatal("a paid plan cannot buy top-ups")
	}
	granted := paid
	granted.Granted = true
	if paidTopupWorkspace(granted, now) {
		t.Fatal("a support grant can buy top-ups")
	}
}
