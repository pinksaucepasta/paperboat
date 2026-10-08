package main

import (
	"context"
	"errors"

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
