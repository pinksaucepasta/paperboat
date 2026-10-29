package main

import (
	"context"
	"errors"
	"github.com/pinksaucepasta/paperboat/internal/diagnostics"
	doctorpkg "github.com/pinksaucepasta/paperboat/internal/doctor"

	"github.com/pinksaucepasta/paperboat/internal/errorreport"
)

var errLocalDoctorHealthInvalid = errors.New("local runtime returned an invalid health response")

// Doctor returns usable diagnostic state even when individual probes fail.
// Keep the original failures as local evidence without exporting state files.
func observeLocalDoctorFailure(ctx context.Context, err error) {
	if err != nil && !errorreport.HTTPAttemptObserved(err) {
		errorreport.Current().ObserveFailure(ctx, "pb", "doctor", "reconciliation", "command_failed", err)
	}
}

// Retain the actual report outcome locally, including checks that have no Go error.
func recordDoctorResult(ctx context.Context, report doctorpkg.Report) {
	recorder := diagnostics.FromContext(ctx)
	if recorder == nil {
		return
	}
	for _, check := range report.Checks {
		if check.Status == doctorpkg.StatusPass || check.Status == doctorpkg.StatusPending {
			continue
		}
		if err := recorder.RecordWithSupportReference("doctor", check.Code, "warning", report.CorrelationID, map[string]string{"state": check.Status, "reason": check.Summary}); err != nil {
			observeLocalDoctorFailure(ctx, err)
		}
	}
}
