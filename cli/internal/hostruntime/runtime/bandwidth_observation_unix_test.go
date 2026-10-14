//go:build !windows

package runtime

import (
	"context"
	"errors"
	"syscall"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/supportref"
)

func TestBandwidthRetryObservationHasOriginalCauseAndCanonicalReference(t *testing.T) {
	ctx := supportref.WithContext(context.Background(), "support_8bade710-6dc8-4b5c-a8b6-289531e85111")
	var faults []errorreport.Fault
	restore := errorreport.InstallFaultObserver(func(_ context.Context, f errorreport.Fault) { faults = append(faults, f) })
	defer restore()
	observe := newBandwidthObservation(ctx)
	observe(syscall.ECONNREFUSED)
	observe(syscall.ECONNREFUSED)
	observe(context.Canceled)
	observe(syscall.ECONNREFUSED)
	if len(faults) != 1 {
		t.Fatalf("duplicate outage faults: %d", len(faults))
	}
	observe(nil)
	observe(syscall.ECONNREFUSED)
	if len(faults) != 2 {
		t.Fatalf("new outage after recovery: %d", len(faults))
	}
	for _, f := range faults {
		if f.Operation != "bandwidth" || f.Stage != "control_request" || f.Code != "control_request_failed" || f.Cause != "connection_refused" || f.SupportReference != supportref.FromContext(ctx) {
			t.Fatalf("invalid projection: %+v", f)
		}
	}
	observe(errors.Join(context.Canceled, syscall.ENOSPC))
	if len(faults) != 3 || faults[2].Cause != "resource_exhausted" || faults[2].Errno != int(syscall.ENOSPC) {
		t.Fatalf("mixed cancellation masked operational failure: %+v", faults)
	}
}
