package localdaemon

import (
	"context"
	"sync"

	"github.com/pinksaucepasta/paperboat/internal/diagnostics"
	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/supportref"
)

// These two daemon-owned retries are optional maintenance, so their outages
// are observed without making the local API unavailable or capturing each tick.
func maintenanceObserver(ctx context.Context, recorder *diagnostics.Recorder, kind string) func(error) {
	operation, stage, code, service, category, event := "ssh", "command", "managed_ssh_failed", "managed_ssh_authority", "ssh", "managed_refresh"
	if kind == "peer_metadata" {
		operation, stage, code, service, category, event = "peer_identity", "peer_authority", "peer_authority_failed", "peer_transport", "transport", "metadata_warm"
	}
	var mu sync.Mutex
	var previous errorreport.Fault
	return func(err error) {
		fault := errorreport.ProjectFault(ctx, "paperboatd", operation, stage, code, err)
		if fault.Outcome == "canceled" {
			return
		}
		mu.Lock()
		old := previous
		previous = fault
		mu.Unlock()
		if err == nil {
			if old.Code != "" {
				_ = recorder.RecordWithSupportReference(category, event, "info", supportref.FromContext(ctx), map[string]string{"outcome": "ready"})
				errorreport.Current().Lifecycle(ctx, service, operation, "recovered", "success")
			}
			return
		}
		if old.Code == fault.Code && old.Stage == fault.Stage && old.Cause == fault.Cause && old.Errno == fault.Errno && old.HTTPStatus == fault.HTTPStatus {
			return
		}
		if !errorreport.HTTPAttemptObserved(err) {
			errorreport.Current().ObserveFailure(ctx, "paperboatd", operation, stage, code, err)
		}
		fields := map[string]string{"outcome": "degraded", "cause": fault.Cause}
		if kind == "managed_ssh" {
			fields["reason"] = ManagedSSHHealthCode(err)
		}
		_ = recorder.RecordWithSupportReference(category, event, "warning", supportref.FromContext(ctx), fields)
	}
}
