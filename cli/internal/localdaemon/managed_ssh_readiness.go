package localdaemon

import (
	"context"
	"github.com/pinksaucepasta/paperboat/internal/diagnostics"
	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/supportref"
)

const managedSSHDoctorRecovery = "Run pb ssh doctor <machine>."

// managedSSHReadinessSource lets the daemon publish local managed-SSH startup
// state without coupling inventory to the authenticated control-plane source.
type managedSSHReadinessSource interface {
	SetManagedSSHReadiness(ready bool, code string)
}

func reportManagedSSHStartup(ctx context.Context, source MachineSource, recorder *diagnostics.Recorder, startupErr error) {
	if startupErr != nil && !errorreport.HTTPAttemptObserved(startupErr) && errorreport.ProjectFault(ctx, "paperboatd", "ssh", "command", "managed_ssh_failed", startupErr).Outcome != "canceled" {
		errorreport.Current().ObserveFailure(ctx, "paperboatd", "ssh", "command", "managed_ssh_failed", startupErr)
	}
	readiness, ok := source.(managedSSHReadinessSource)
	if !ok {
		return
	}
	if startupErr == nil {
		readiness.SetManagedSSHReadiness(true, "")
		return
	}
	code := ManagedSSHHealthCode(startupErr)
	readiness.SetManagedSSHReadiness(false, code)
	// Do not record startupErr: it can contain operating-system or credential
	// details. The typed code and recovery action are sufficient for support.
	_ = recorder.RecordWithSupportReference("ssh", "managed_startup", "warning", supportref.FromContext(ctx), map[string]string{
		"outcome": "degraded",
		"reason":  code,
	})
}
