package configsync

import (
	"context"
	"sync"

	"github.com/pinksaucepasta/paperboat/internal/diagnostics"
	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/supportref"
)

const (
	configSyncComponent   = "paperboat-daemon"
	configSyncOperation   = "config_sync"
	configSyncFailureCode = "config_sync_failed"
)

// configSyncFailureObservation retains only the latest bounded fault class.
// Retry owners use it to suppress repeated local records and emit a recovery
// only after the same operation becomes usable again.
type configSyncFailureObservation struct {
	mu   sync.Mutex
	last errorreport.Fault
}

func (o *configSyncFailureObservation) observe(ctx context.Context, stage string, err error) {
	if err == nil {
		return
	}
	fault := errorreport.ProjectFault(ctx, configSyncComponent, configSyncOperation, stage, configSyncFailureCode, err)
	if fault.Code == "" || fault.Outcome == "canceled" {
		return
	}
	o.mu.Lock()
	previous := o.last
	unchanged := sameConfigSyncFault(previous, fault)
	o.last = fault
	o.mu.Unlock()
	if unchanged || errorreport.HTTPAttemptObserved(err) {
		return
	}
	if fault.SupportReference != "" {
		ctx = supportref.WithContext(ctx, fault.SupportReference)
	}
	errorreport.Current().ObserveFailure(ctx, configSyncComponent, configSyncOperation, stage, configSyncFailureCode, err)
}

func (o *configSyncFailureObservation) recovered(ctx context.Context) {
	o.mu.Lock()
	previous := o.last
	o.last = errorreport.Fault{}
	o.mu.Unlock()
	if previous.Code == "" {
		return
	}
	if previous.SupportReference != "" {
		ctx = supportref.WithContext(ctx, previous.SupportReference)
	}
	errorreport.Current().Lifecycle(ctx, "config", configSyncOperation, "recovered", "success")
	if recorder := diagnostics.FromContext(ctx); recorder != nil {
		_ = recorder.RecordWithSupportReference(previous.Stage, "recovered", "info", previous.SupportReference, map[string]string{
			"component": configSyncComponent,
			"operation": configSyncOperation,
		})
	}
}

func sameConfigSyncFault(left, right errorreport.Fault) bool {
	return left.Code != "" && left.Code == right.Code && left.Stage == right.Stage &&
		left.Cause == right.Cause && left.Errno == right.Errno && left.HTTPStatus == right.HTTPStatus
}
