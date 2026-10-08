package localdaemon

import (
	"context"
	"sync"

	"github.com/pinksaucepasta/paperboat/internal/diagnostics"
	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/supportref"
)

func inventoryRefreshObserver(ctx context.Context, recorder *diagnostics.Recorder) func(error) {
	var mu sync.Mutex
	var previous errorreport.Fault
	return func(err error) {
		fault := errorreport.ProjectFault(ctx, "paperboatd", "machine_control", "reconciliation", "control_request_failed", err)
		if fault.Outcome == "canceled" {
			return
		}
		mu.Lock()
		old := previous
		previous = fault
		mu.Unlock()
		if err == nil {
			if old.Code != "" {
				_ = recorder.RecordWithSupportReference("reconciliation", "inventory_refresh", "info", supportref.FromContext(ctx), map[string]string{"outcome": "ready"})
				errorreport.Current().Lifecycle(ctx, "control_plane", "machine_control", "recovered", "success")
			}
			return
		}
		if old.Code == fault.Code && old.Stage == fault.Stage && old.Cause == fault.Cause && old.Errno == fault.Errno && old.HTTPStatus == fault.HTTPStatus {
			return
		}
		if !errorreport.HTTPAttemptObserved(err) {
			errorreport.Current().ObserveFailure(ctx, "paperboatd", "machine_control", "reconciliation", "control_request_failed", err)
		}
		severity, fields := inventoryRefreshDiagnostic(err)
		fields["cause"] = fault.Cause
		_ = recorder.RecordWithSupportReference("reconciliation", "inventory_refresh", severity, supportref.FromContext(ctx), fields)
	}
}
