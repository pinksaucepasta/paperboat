//go:build darwin || linux || windows

package runtime

import (
	"context"
	"sync"

	"github.com/pinksaucepasta/paperboat/internal/diagnostics"
	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/supportref"
)

// The recorder retries indefinitely; retain one bounded classification rather
// than capturing an exception for each tick of the same outage.
func newBandwidthObservation(ctx context.Context) func(error) {
	ctx = context.WithoutCancel(ctx)
	var mu sync.Mutex
	var previous errorreport.Fault
	return func(err error) {
		fault := errorreport.ProjectFault(ctx, "paperboat-daemon", "bandwidth", "control_request", "control_request_failed", err)
		if fault.Outcome == "canceled" {
			return
		}
		mu.Lock()
		old := previous
		if err == nil {
			previous = errorreport.Fault{}
		} else {
			previous = fault
		}
		mu.Unlock()
		if err == nil {
			if old.Code != "" {
				errorreport.Current().Lifecycle(ctx, "control_plane", "bandwidth", "recovered", "success")
				if local := diagnostics.FromContext(ctx); local != nil {
					_ = local.RecordWithSupportReference(old.Stage, "recovered", "info", supportref.FromContext(ctx), map[string]string{"component": "paperboat-daemon", "operation": "bandwidth"})
				}
			}
			return
		}
		if old.Code == fault.Code && old.Stage == fault.Stage && old.Cause == fault.Cause && old.Errno == fault.Errno && old.HTTPStatus == fault.HTTPStatus {
			return
		}
		if !errorreport.HTTPAttemptObserved(err) {
			errorreport.Current().ObserveFailure(ctx, "paperboat-daemon", "bandwidth", "control_request", "control_request_failed", err)
		}
	}
}
